// Package anthropic 实现 Anthropic Messages 协议（POST /v1/messages + SSE）的流式驱动。
//
// 与 OpenAI Chat Completions 协议的主要差异：
//   - 鉴权走 x-api-key + anthropic-version 头，而非 Authorization: Bearer
//   - system 提示词是顶层字段，不作为 messages 里的一条消息
//   - 工具调用不是 message.tool_calls，而是 assistant 内容块 tool_use / user 内容块 tool_result
//   - 工具 schema 用 input_schema 取代 function.parameters
//   - 流式返回为具名事件（content_block_start / content_block_delta / message_delta 等）
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"gp/internal/ai"
)

var _ ai.Provider = (*Provider)(nil)

const (
	defaultBaseURL   = "https://api.anthropic.com"
	apiVersion       = "2023-06-01"
	defaultMaxTokens = 8192
	// maxCachedSignatures 限制思考签名缓存规模，避免长会话无限增长
	maxCachedSignatures = 512
)

// Provider 实现基于 Anthropic Messages 协议的大模型驱动
type Provider struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client

	// sigMu 保护 signatures：缓存「思考块签名」，用于多轮工具调用时回传 thinking 块
	sigMu      sync.Mutex
	signatures map[string]string
}

// NewProvider 创建 Anthropic 协议 Provider 实例
func NewProvider(apiKey, baseURL string) *Provider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	return &Provider{
		apiKey:     apiKey,
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		httpClient: &http.Client{},
		signatures: make(map[string]string),
	}
}

// SetHTTPClient 注入自定义 HTTP 客户端（主要用于测试与 Mock）
func (p *Provider) SetHTTPClient(client *http.Client) {
	if client != nil {
		p.httpClient = client
	}
}

// Name 返回 Provider 唯一名称标识
func (p *Provider) Name() string {
	return "anthropic"
}

// messagesEndpoint 归一化出 /v1/messages 端点
func (p *Provider) messagesEndpoint() string {
	endpoint := p.baseURL
	if strings.HasSuffix(endpoint, "/messages") {
		return endpoint
	}
	if !strings.HasSuffix(endpoint, "/v1") {
		endpoint += "/v1"
	}
	return endpoint + "/messages"
}

// StreamChat 发起流式推理，返回只读的 StreamChunk 管道
func (p *Provider) StreamChat(ctx context.Context, req *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	if req == nil {
		return nil, fmt.Errorf("chat request cannot be nil")
	}

	body := p.buildRequest(req)
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	resp, err := p.post(ctx, payload)
	if err != nil {
		return nil, err
	}

	// 思考块缺少合法签名时 Anthropic 会直接 400；此处降级为不带 thinking 重试一次
	if resp.StatusCode == http.StatusBadRequest && body.Thinking != nil {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close()
		if strings.Contains(strings.ToLower(string(errBody)), "thinking") {
			body.Thinking = nil
			if retryPayload, mErr := json.Marshal(body); mErr == nil {
				if retryResp, rErr := p.post(ctx, retryPayload); rErr == nil {
					resp = retryResp
				} else {
					return nil, rErr
				}
			}
		} else {
			return nil, fmt.Errorf("anthropic api error (status %d): %s", http.StatusBadRequest, strings.TrimSpace(string(errBody)))
		}
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("anthropic api error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	out := make(chan ai.StreamChunk, 64)
	state := &streamState{
		provider:  p,
		signature: make(map[int]string),
	}
	go p.parseSSEStream(ctx, resp.Body, state, out)

	return out, nil
}

func (p *Provider) post(ctx context.Context, payload []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.messagesEndpoint(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("anthropic-version", apiVersion)
	if p.apiKey != "" {
		httpReq.Header.Set("x-api-key", p.apiKey)
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic stream chat request failed: %w", err)
	}
	return resp, nil
}

// ---------- 请求构造 ----------

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Temperature *float32           `json:"temperature,omitempty"`
	Thinking    *thinkingConfig    `json:"thinking,omitempty"`
	Stream      bool               `json:"stream"`
}

type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type thinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

func (p *Provider) buildRequest(req *ai.ChatRequest) *anthropicRequest {
	system, messages := p.buildMessages(req)

	maxTokens := defaultMaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}

	var thinking *thinkingConfig
	var temperature *float32
	if budget := thinkingBudget(req.ThinkingLevel); budget > 0 {
		// Anthropic 要求 max_tokens 必须大于 thinking.budget_tokens
		if maxTokens <= budget {
			maxTokens = budget + 4096
		}
		thinking = &thinkingConfig{Type: "enabled", BudgetTokens: budget}
	} else {
		// 开启 thinking 时不允许自定义 temperature
		temperature = req.Temperature
	}

	return &anthropicRequest{
		Model:       req.Model,
		MaxTokens:   maxTokens,
		System:      system,
		Messages:    messages,
		Tools:       convertTools(req.Tools),
		Temperature: temperature,
		Thinking:    thinking,
		Stream:      true,
	}
}

// buildMessages 将统一消息模型转换为 Anthropic 的 system 字符串 + 交替消息序列
func (p *Provider) buildMessages(req *ai.ChatRequest) (string, []anthropicMessage) {
	var systemParts []string
	messages := make([]anthropicMessage, 0, len(req.Messages))

	for _, msg := range req.Messages {
		switch msg.Role {
		case ai.RoleSystem:
			if strings.TrimSpace(msg.Content) != "" {
				systemParts = append(systemParts, msg.Content)
			}
		case ai.RoleTool:
			toolContent := []map[string]any{{
				"type": "text",
				"text": msg.Content,
			}}
			for _, img := range msg.Images {
				toolContent = append(toolContent, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": img.MimeType,
						"data":       img.Base64Data,
					},
				})
			}
			appendBlocks(&messages, "user", []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": msg.ToolCallID,
				"content":     toolContent,
			}})
		case ai.RoleAssistant:
			if blocks := p.assistantBlocks(msg); len(blocks) > 0 {
				appendBlocks(&messages, "assistant", blocks)
			}
		default:
			if strings.TrimSpace(msg.Content) == "" && len(msg.Images) == 0 {
				continue
			}
			var userBlocks []map[string]any
			if strings.TrimSpace(msg.Content) != "" {
				userBlocks = append(userBlocks, map[string]any{
					"type": "text",
					"text": msg.Content,
				})
			}
			for _, img := range msg.Images {
				userBlocks = append(userBlocks, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": img.MimeType,
						"data":       img.Base64Data,
					},
				})
			}
			appendBlocks(&messages, "user", userBlocks)
		}
	}

	return strings.Join(systemParts, "\n\n"), messages
}

// assistantBlocks 将助手历史消息还原为内容块（thinking / text / tool_use）
func (p *Provider) assistantBlocks(msg ai.Message) []map[string]any {
	ids := make([]string, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		ids = append(ids, tc.ID)
	}

	blocks := make([]map[string]any, 0, len(msg.ToolCalls)+2)
	if strings.TrimSpace(msg.Thinking) != "" {
		// thinking 块必须带签名回传，否则协议校验失败；优先使用消息上持久化的 ThinkingSignature，再回退内存缓存
		sig := msg.ThinkingSignature
		if sig == "" {
			sig = p.lookupSignature(signatureKey(msg.Content, msg.Thinking, ids))
		}
		if sig != "" {
			blocks = append(blocks, map[string]any{
				"type":      "thinking",
				"thinking":  msg.Thinking,
				"signature": sig,
			})
		}
	}
	if strings.TrimSpace(msg.Content) != "" {
		blocks = append(blocks, map[string]any{
			"type": "text",
			"text": msg.Content,
		})
	}
	for _, tc := range msg.ToolCalls {
		input := json.RawMessage(tc.Function.Arguments)
		if len(bytes.TrimSpace(input)) == 0 || !json.Valid(input) {
			input = json.RawMessage("{}")
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Function.Name,
			"input": input,
		})
	}
	return blocks
}

// appendBlocks 追加消息；Anthropic 要求 user/assistant 交替，同角色连续消息合并为一轮
func appendBlocks(messages *[]anthropicMessage, role string, blocks []map[string]any) {
	if len(blocks) == 0 {
		return
	}
	if n := len(*messages); n > 0 && (*messages)[n-1].Role == role {
		existing := (*messages)[n-1].Content
		if role == "user" && blocks[0]["type"] == "tool_result" {
			// tool_result 必须排在同一轮 user 消息的最前面，否则协议校验失败
			insertAt := 0
			for insertAt < len(existing) && existing[insertAt]["type"] == "tool_result" {
				insertAt++
			}
			merged := make([]map[string]any, 0, len(existing)+len(blocks))
			merged = append(merged, existing[:insertAt]...)
			merged = append(merged, blocks...)
			merged = append(merged, existing[insertAt:]...)
			(*messages)[n-1].Content = merged
			return
		}
		(*messages)[n-1].Content = append(existing, blocks...)
		return
	}
	*messages = append(*messages, anthropicMessage{Role: role, Content: blocks})
}

// convertTools 将 OpenAI function 工具定义转换为 Anthropic tools 定义
func convertTools(tools []json.RawMessage) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}

	out := make([]anthropicTool, 0, len(tools))
	for _, raw := range tools {
		var oa struct {
			Function *struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal(raw, &oa); err != nil {
			continue
		}

		tool := anthropicTool{Name: oa.Name, Description: oa.Description, InputSchema: oa.InputSchema}
		if oa.Function != nil {
			tool.Name = oa.Function.Name
			tool.Description = oa.Function.Description
			tool.InputSchema = oa.Function.Parameters
		}
		if tool.Name == "" {
			continue
		}
		if len(bytes.TrimSpace(tool.InputSchema)) == 0 || !json.Valid(tool.InputSchema) {
			tool.InputSchema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, tool)
	}
	return out
}

// thinkingBudget 将统一思考档位映射为 Anthropic 的思考 Token 预算
func thinkingBudget(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "minimal":
		return 1024
	case "low":
		return 2048
	case "medium", "on":
		return 8192
	case "high":
		return 16384
	case "xhigh", "max":
		return 32768
	default:
		return 0
	}
}

// ---------- 思考签名缓存 ----------

func signatureKey(content, thinking string, toolCallIDs []string) string {
	hasher := sha256.New()
	hasher.Write([]byte(content))
	hasher.Write([]byte{0})
	hasher.Write([]byte(thinking))
	for _, id := range toolCallIDs {
		hasher.Write([]byte{0})
		hasher.Write([]byte(id))
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func (p *Provider) storeSignature(key, signature string) {
	if key == "" || signature == "" {
		return
	}
	p.sigMu.Lock()
	defer p.sigMu.Unlock()
	if p.signatures == nil || len(p.signatures) >= maxCachedSignatures {
		p.signatures = make(map[string]string)
	}
	p.signatures[key] = signature
}

func (p *Provider) lookupSignature(key string) string {
	p.sigMu.Lock()
	defer p.sigMu.Unlock()
	return p.signatures[key]
}

// ---------- 流式解析 ----------

type sseEvent struct {
	Type    string `json:"type"`
	Index   *int   `json:"index"`
	Message *struct {
		Usage *sseUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Name      string `json:"name"`
		Signature string `json:"signature"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *sseUsage `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type sseUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type streamState struct {
	provider *Provider

	inputTokens      int64
	outputTokens     int64
	cacheReadTokens  int64
	cacheWriteTokens int64

	text      strings.Builder
	thinking  strings.Builder
	signature map[int]string
	toolIDs   []string
}

func (p *Provider) parseSSEStream(ctx context.Context, body io.ReadCloser, state *streamState, out chan<- ai.StreamChunk) {
	defer close(out)
	defer body.Close()

	reader := bufio.NewReader(body)
	for {
		lineBytes, err := reader.ReadBytes('\n')
		if len(lineBytes) > 0 {
			line := bytes.TrimSpace(lineBytes)
			// event: 行可忽略，Anthropic 每个 data 载荷内都带 "type"
			if bytes.HasPrefix(line, []byte("data:")) {
				payload := bytes.TrimSpace(line[len("data:"):])
				if len(payload) > 0 && state.handle(ctx, payload, out) {
					return
				}
			}
		}

		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				state.emit(ctx, out, ai.StreamChunk{Error: fmt.Errorf("reading sse stream: %w", err)})
			}
			return
		}
	}
}

// handle 处理单个 SSE 事件，返回 true 表示流已终止
func (s *streamState) handle(ctx context.Context, payload []byte, out chan<- ai.StreamChunk) bool {
	var ev sseEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		s.emit(ctx, out, ai.StreamChunk{Error: fmt.Errorf("unmarshal sse payload: %w", err)})
		return true
	}

	switch ev.Type {
	case "message_start":
		if ev.Message != nil && ev.Message.Usage != nil {
			s.absorbUsage(ev.Message.Usage)
		}
	case "content_block_start":
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			idx := eventIndex(ev.Index)
			s.toolIDs = append(s.toolIDs, ev.ContentBlock.ID)
			idxCopy := idx
			s.emit(ctx, out, ai.StreamChunk{ToolCallDelta: &ai.ToolCall{
				Index:    &idxCopy,
				ID:       ev.ContentBlock.ID,
				Type:     "function",
				Function: ai.FunctionCall{Name: ev.ContentBlock.Name},
			}})
		}
	case "content_block_delta":
		if ev.Delta == nil {
			return false
		}
		idx := eventIndex(ev.Index)
		switch ev.Delta.Type {
		case "text_delta":
			s.text.WriteString(ev.Delta.Text)
			s.emit(ctx, out, ai.StreamChunk{DeltaText: ev.Delta.Text})
		case "thinking_delta":
			s.thinking.WriteString(ev.Delta.Thinking)
			s.emit(ctx, out, ai.StreamChunk{ThinkingDelta: ev.Delta.Thinking})
		case "signature_delta":
			s.signature[idx] += ev.Delta.Signature
			s.emit(ctx, out, ai.StreamChunk{ThinkingSignature: ev.Delta.Signature})
		case "input_json_delta":
			idxCopy := idx
			s.emit(ctx, out, ai.StreamChunk{ToolCallDelta: &ai.ToolCall{
				Index:    &idxCopy,
				Type:     "function",
				Function: ai.FunctionCall{Arguments: json.RawMessage(ev.Delta.PartialJSON)},
			}})
		}
	case "message_delta":
		if ev.Usage != nil {
			s.absorbUsage(ev.Usage)
		}
		finishReason := mapStopReason(stopReasonOf(ev))
		s.rememberSignature()
		s.emit(ctx, out, ai.StreamChunk{
			FinishReason: finishReason,
			Usage:        s.tokenUsage(),
		})
	case "message_stop":
		s.rememberSignature()
		return true
	case "error":
		if ev.Error != nil {
			s.emit(ctx, out, ai.StreamChunk{Error: fmt.Errorf("anthropic stream error (%s): %s", ev.Error.Type, ev.Error.Message)})
		} else {
			s.emit(ctx, out, ai.StreamChunk{Error: fmt.Errorf("anthropic stream error: %s", string(payload))})
		}
		return true
	}

	return false
}

func (s *streamState) absorbUsage(usage *sseUsage) {
	if usage == nil {
		return
	}
	if usage.InputTokens > 0 {
		s.inputTokens = usage.InputTokens
	}
	if usage.OutputTokens > 0 {
		s.outputTokens = usage.OutputTokens
	}
	if usage.CacheReadInputTokens > 0 {
		s.cacheReadTokens = usage.CacheReadInputTokens
	}
	if usage.CacheCreationInputTokens > 0 {
		s.cacheWriteTokens = usage.CacheCreationInputTokens
	}
}

func (s *streamState) tokenUsage() *ai.TokenUsage {
	totalInput := s.inputTokens + s.cacheReadTokens + s.cacheWriteTokens
	return &ai.TokenUsage{
		InputTokens:      totalInput,
		OutputTokens:     s.outputTokens,
		CacheReadTokens:  s.cacheReadTokens,
		CacheWriteTokens: s.cacheWriteTokens,
		TotalTokens:      totalInput + s.outputTokens,
	}
}

// rememberSignature 缓存本轮思考块签名，供多轮工具调用回传
func (s *streamState) rememberSignature() {
	signature := ""
	for idx := 0; idx < len(s.signature); idx++ {
		signature += s.signature[idx]
	}
	if signature == "" {
		return
	}

	s.provider.storeSignature(signatureKey(s.text.String(), s.thinking.String(), s.toolIDs), signature)
}

func (s *streamState) emit(ctx context.Context, out chan<- ai.StreamChunk, chunk ai.StreamChunk) {
	select {
	case out <- chunk:
	case <-ctx.Done():
	}
}

func eventIndex(idx *int) int {
	if idx == nil {
		return 0
	}
	return *idx
}

func stopReasonOf(ev sseEvent) string {
	if ev.Delta == nil {
		return ""
	}
	return ev.Delta.StopReason
}

// mapStopReason 将 Anthropic 的 stop_reason 归一化为 OpenAI 风格的 finish_reason
func mapStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	default:
		return ""
	}
}
