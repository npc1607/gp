package qoder

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"gp/internal/ai"
)

var _ ai.Provider = (*Provider)(nil)

const chatPath = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

// Provider 实现基于 Qoder 原生上游协议（PAT -> jobToken -> COSY 签名 + SSE Envelope）的大模型驱动
type Provider struct {
	pat        string
	httpClient *http.Client
	realms     []realmEndpoint

	mu        sync.Mutex
	cached    *exchangedAuth
	sessionID string
}

// NewProvider 创建 Qoder 原生协议 Provider 实例
func NewProvider(pat string) *Provider {
	return &Provider{
		pat:        strings.TrimSpace(pat),
		httpClient: &http.Client{},
		realms:     defaultRealms,
		sessionID:  randomUUID(),
	}
}

// SetHTTPClient 注入自定义 HTTP 客户端（用于单元测试）
func (p *Provider) SetHTTPClient(c *http.Client) {
	if c != nil {
		p.httpClient = c
	}
}

// SetRealms 注入自定义 Realm 列表（用于单元测试）
func (p *Provider) SetRealms(r []realmEndpoint) {
	if len(r) > 0 {
		p.realms = r
	}
}

// Name 返回 Provider 标识
func (p *Provider) Name() string {
	return "qoder"
}

func (p *Provider) getAuth(ctx context.Context, forceRefresh bool) (*exchangedAuth, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !forceRefresh && p.cached != nil && time.Now().Before(p.cached.Expires.Add(-1*time.Minute)) {
		return p.cached, nil
	}
	ex, err := exchangePAT(ctx, p.httpClient, p.pat, p.realms)
	if err != nil {
		return nil, err
	}
	p.cached = ex
	return ex, nil
}

// StreamChat 发起流式推理，返回只读 StreamChunk 管道
func (p *Provider) StreamChat(ctx context.Context, req *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	if req == nil {
		return nil, fmt.Errorf("chat request cannot be nil")
	}
	authInfo, err := p.getAuth(ctx, false)
	if err != nil {
		return nil, err
	}

	resp, err := p.doChatRequest(ctx, authInfo, req)
	if err != nil && (strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "HTTP 403")) {
		if refreshed, rErr := p.getAuth(ctx, true); rErr == nil {
			authInfo = refreshed
			resp, err = p.doChatRequest(ctx, authInfo, req)
		}
	}
	if err != nil {
		return nil, err
	}

	ch := make(chan ai.StreamChunk, 64)
	go p.readSSEStream(ctx, resp.Body, ch)
	return ch, nil
}

func (p *Provider) doChatRequest(ctx context.Context, authInfo *exchangedAuth, req *ai.ChatRequest) (*http.Response, error) {
	modelKey := strings.TrimSpace(req.Model)
	modelKey = strings.TrimPrefix(modelKey, "qoder/")
	modelKey = strings.TrimPrefix(modelKey, "qcorder/")
	if modelKey == "" {
		modelKey = "auto"
	}

	bodyMap := p.buildBody(authInfo, req, modelKey)
	plainJSON, err := json.Marshal(bodyMap)
	if err != nil {
		return nil, fmt.Errorf("marshal qoder body: %w", err)
	}
	encoded := EncodeBody(plainJSON)
	rawURL := strings.TrimRight(authInfo.Gateway, "/") + chatPath
	headers := authInfo.Session.Headers(encoded, rawURL, modelKey)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewBufferString(encoded))
	if err != nil {
		return nil, fmt.Errorf("create qoder request: %w", err)
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("qoder request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rawErr, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("qoder upstream error (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(rawErr)))
	}
	return resp, nil
}

func (p *Provider) buildBody(authInfo *exchangedAuth, req *ai.ChatRequest, modelKey string) map[string]any {
	reqID := randomUUID()
	var msgs []map[string]any
	var lastPrompt string

	for _, m := range req.Messages {
		entry := map[string]any{
			"role":    string(m.Role),
			"content": m.Content,
		}
		if m.Thinking != "" {
			entry["reasoning_content"] = m.Thinking
		}
		if len(m.ToolCalls) > 0 {
			var tcs []map[string]any
			for _, tc := range m.ToolCalls {
				tcs = append(tcs, map[string]any{
					"id":   tc.ID,
					"type": tc.Type,
					"function": map[string]any{
						"name":      tc.Function.Name,
						"arguments": string(tc.Function.Arguments),
					},
				})
			}
			entry["tool_calls"] = tcs
		}
		if m.ToolCallID != "" {
			entry["tool_call_id"] = m.ToolCallID
		}
		msgs = append(msgs, entry)
		if m.Role == ai.RoleUser && m.Content != "" {
			lastPrompt = m.Content
		}
	}

	var tools []any
	for _, rawT := range req.Tools {
		var parsed any
		if json.Unmarshal(rawT, &parsed) == nil {
			tools = append(tools, parsed)
		}
	}

	maxTokens := 32768
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}
	params := map[string]any{
		"max_tokens": maxTokens,
	}
	if req.Temperature != nil {
		params["temperature"] = *req.Temperature
	}

	isReasoning := req.ThinkingLevel != "" && req.ThinkingLevel != "off"

	body := map[string]any{
		"request_id":       reqID,
		"request_set_id":   randomUUID(),
		"chat_record_id":   reqID,
		"session_id":       p.sessionID,
		"stream":           true,
		"chat_task":        "FREE_INPUT",
		"code_language":    "",
		"source":           1,
		"version":          "3",
		"is_reply":         true,
		"is_retry":         false,
		"chat_prompt":      "",
		"aliyun_user_type": authInfo.Session.UserType,
		"session_type":     "qodercli",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"parameters":       params,
		"messages":         msgs,
		"chat_context": map[string]any{
			"chatPrompt": "",
			"features":   []any{},
			"imageUrls":  nil,
			"text": map[string]any{
				"type": "text",
				"text": lastPrompt,
			},
			"extra": map[string]any{
				"context": []any{},
				"modelConfig": map[string]any{
					"is_reasoning": isReasoning,
					"key":          modelKey,
				},
				"originalContent": map[string]any{
					"type": "text",
					"text": lastPrompt,
				},
			},
		},
		"model_config": map[string]any{
			"key":               modelKey,
			"display_name":      modelKey,
			"model":             "",
			"format":            "openai",
			"is_vl":             false,
			"is_reasoning":      isReasoning,
			"api_key":           "",
			"url":               "",
			"source":            "system",
			"max_input_tokens":  200000,
		},
		"business": map[string]any{
			"product":  "cli",
			"version":  "1.1.75",
			"type":     "agent",
			"id":       randomUUID(),
			"name":     lastPrompt,
			"begin_at": time.Now().UnixMilli(),
			"stage":    "start",
		},
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	return body
}

func (p *Provider) readSSEStream(ctx context.Context, body io.ReadCloser, ch chan<- ai.StreamChunk) {
	defer close(ch)
	defer func() { _ = body.Close() }()

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		if ctx.Err() != nil {
			ch <- ai.StreamChunk{Error: ctx.Err()}
			return
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		for strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(line[len("data:"):])
		}
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if line == "[DONE]" {
			return
		}

		// Qoder 上游外层 Envelope：{"headers": ..., "body": "<json>", "statusCodeValue": 200}
		var outer struct {
			Body            any `json:"body"`
			StatusCodeValue any `json:"statusCodeValue"`
		}
		if err := json.Unmarshal([]byte(line), &outer); err != nil {
			continue
		}
		if st := parseStatusCode(outer.StatusCodeValue); st != 0 && st != 200 {
			detail := fmt.Sprintf("%v", outer.Body)
			ch <- ai.StreamChunk{Error: fmt.Errorf("qoder upstream status %d: %s", st, detail)}
			return
		}
		innerStr, ok := outer.Body.(string)
		if !ok {
			continue
		}
		if innerStr == "[DONE]" {
			return
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Thinking         string `json:"thinking"`
					ToolCalls        []struct {
						Index    *int   `json:"index,omitempty"`
						ID       string `json:"id,omitempty"`
						Type     string `json:"type,omitempty"`
						Function *struct {
							Name      string `json:"name,omitempty"`
							Arguments string `json:"arguments,omitempty"`
						} `json:"function,omitempty"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				TotalTokens      int64 `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(innerStr), &chunk); err != nil {
			continue
		}

		var out ai.StreamChunk
		var hasData bool
		if len(chunk.Choices) > 0 {
			d := chunk.Choices[0].Delta
			if d.Content != "" {
				out.DeltaText = d.Content
				hasData = true
			}
			if d.ReasoningContent != "" {
				out.ThinkingDelta = d.ReasoningContent
				hasData = true
			} else if d.Thinking != "" {
				out.ThinkingDelta = d.Thinking
				hasData = true
			}
			if chunk.Choices[0].FinishReason != "" {
				out.FinishReason = chunk.Choices[0].FinishReason
				hasData = true
			}
			for _, tc := range d.ToolCalls {
				var fnName, fnArgs string
				if tc.Function != nil {
					fnName = tc.Function.Name
					fnArgs = tc.Function.Arguments
				}
				scChunk := ai.StreamChunk{
					ToolCallDelta: &ai.ToolCall{
						Index: tc.Index,
						ID:    tc.ID,
						Type:  tc.Type,
						Function: ai.FunctionCall{
							Name:      fnName,
							Arguments: json.RawMessage(fnArgs),
						},
					},
				}
				ch <- scChunk
			}
		}
		if chunk.Usage != nil {
			out.Usage = &ai.TokenUsage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
				TotalTokens:  chunk.Usage.TotalTokens,
			}
			hasData = true
		}
		if hasData {
			ch <- out
		}
	}

	if err := sc.Err(); err != nil && ctx.Err() == nil {
		ch <- ai.StreamChunk{Error: fmt.Errorf("qoder stream read error: %w", err)}
	}
}

func parseStatusCode(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}
