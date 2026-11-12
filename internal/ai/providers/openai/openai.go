package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"gp/internal/ai"
)

var _ ai.Provider = (*Provider)(nil)

const defaultBaseURL = "https://api.openai.com/v1"

// Provider 实现基于 OpenAI 兼容协议的大模型驱动
type Provider struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// NewProvider 创建 OpenAI 兼容协议 Provider 实例
func NewProvider(apiKey, baseURL string) *Provider {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")

	return &Provider{
		apiKey:     apiKey,
		baseURL:    baseURL,
		httpClient: &http.Client{},
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
	return "openai"
}

// StreamChat 发起流式推理，返回只读的 StreamChunk 管道
func (p *Provider) StreamChat(ctx context.Context, req *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	if req == nil {
		return nil, fmt.Errorf("chat request cannot be nil")
	}

	endpoint := p.baseURL
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint = endpoint + "/chat/completions"
	}

	payload, err := p.buildRequestBody(req)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai stream chat request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("openai api error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	out := make(chan ai.StreamChunk, 64)
	go p.parseSSEStream(ctx, resp.Body, out)

	return out, nil
}

type openAIChatRequest struct {
	Model         string            `json:"model"`
	Messages      []openAIMessage   `json:"messages"`
	Tools         []json.RawMessage `json:"tools,omitempty"`
	Stream        bool              `json:"stream"`
	StreamOptions *streamOptions    `json:"stream_options,omitempty"`
	Temperature   *float32          `json:"temperature,omitempty"`
	MaxTokens     *int              `json:"max_tokens,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

func (p *Provider) buildRequestBody(req *ai.ChatRequest) ([]byte, error) {
	messages := make([]openAIMessage, 0, len(req.Messages))
	for _, msg := range req.Messages {
		oMsg := openAIMessage{
			Role:    string(msg.Role),
			Content: msg.Content,
		}
		if msg.Role == ai.RoleTool {
			oMsg.ToolCallID = msg.ToolCallID
		}
		if len(msg.ToolCalls) > 0 {
			oMsg.ToolCalls = make([]openAIToolCall, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				tcType := tc.Type
				if tcType == "" {
					tcType = "function"
				}
				oMsg.ToolCalls = append(oMsg.ToolCalls, openAIToolCall{
					ID:   tc.ID,
					Type: tcType,
					Function: openAIFunctionCall{
						Name:      tc.Function.Name,
						Arguments: string(tc.Function.Arguments),
					},
				})
			}
		}
		messages = append(messages, oMsg)
	}

	oReq := openAIChatRequest{
		Model:         req.Model,
		Messages:      messages,
		Tools:         req.Tools,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		Temperature:   req.Temperature,
		MaxTokens:     req.MaxTokens,
	}

	return json.Marshal(oReq)
}

type openAIStreamChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string                `json:"role,omitempty"`
			Content          string                `json:"content,omitempty"`
			ReasoningContent string                `json:"reasoning_content,omitempty"`
			ToolCalls        []openAIToolCallDelta `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage,omitempty"`
}

type openAIToolCallDelta struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function *struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

type openAIUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

func (p *Provider) parseSSEStream(ctx context.Context, body io.ReadCloser, out chan<- ai.StreamChunk) {
	defer close(out)
	defer body.Close()

	reader := bufio.NewReader(body)

	for {
		lineBytes, err := reader.ReadBytes('\n')
		if len(lineBytes) > 0 {
			line := bytes.TrimRight(lineBytes, "\r\n")
			line = bytes.TrimSpace(line)

			// SSE comments or empty lines
			if len(line) == 0 || bytes.HasPrefix(line, []byte(":")) {
				if err != nil {
					break
				}
				continue
			}

			if bytes.HasPrefix(line, []byte("data:")) {
				payload := bytes.TrimSpace(line[len("data:"):])
				if string(payload) == "[DONE]" {
					return
				}

				var rawChunk openAIStreamChunk
				if jsonErr := json.Unmarshal(payload, &rawChunk); jsonErr != nil {
					select {
					case out <- ai.StreamChunk{Error: fmt.Errorf("unmarshal sse payload: %w", jsonErr)}:
					case <-ctx.Done():
					}
					return
				}

				p.dispatchSSEChunk(ctx, &rawChunk, out)
			}
		}

		if err != nil {
			if err != io.EOF && ctx.Err() == nil {
				select {
				case out <- ai.StreamChunk{Error: fmt.Errorf("reading sse stream: %w", err)}:
				case <-ctx.Done():
				}
			}
			break
		}
	}
}

func (p *Provider) dispatchSSEChunk(ctx context.Context, chunk *openAIStreamChunk, out chan<- ai.StreamChunk) {
	// 1. Process usage if present (often in the terminal chunk with empty choices)
	if chunk.Usage != nil {
		usage := &ai.TokenUsage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens:  chunk.Usage.TotalTokens,
		}
		if chunk.Usage.PromptTokensDetails != nil {
			usage.CacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
		}
		select {
		case out <- ai.StreamChunk{Usage: usage}:
		case <-ctx.Done():
			return
		}
	}

	// 2. Process choices
	for _, choice := range chunk.Choices {
		finishReason := ""
		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}

		// Text content / Reasoning content
		if choice.Delta.Content != "" || choice.Delta.ReasoningContent != "" || (finishReason != "" && len(choice.Delta.ToolCalls) == 0) {
			sc := ai.StreamChunk{
				DeltaText:     choice.Delta.Content,
				ThinkingDelta: choice.Delta.ReasoningContent,
				FinishReason:  finishReason,
			}
			select {
			case out <- sc:
			case <-ctx.Done():
				return
			}
		}

		// Tool calls deltas
		for _, tcDelta := range choice.Delta.ToolCalls {
			tc := &ai.ToolCall{
				Index: tcDelta.Index,
				ID:    tcDelta.ID,
				Type:  tcDelta.Type,
			}
			if tcDelta.Function != nil {
				tc.Function = ai.FunctionCall{
					Name:      tcDelta.Function.Name,
					Arguments: json.RawMessage(tcDelta.Function.Arguments),
				}
			}
			sc := ai.StreamChunk{
				ToolCallDelta: tc,
				FinishReason:  finishReason,
			}
			select {
			case out <- sc:
			case <-ctx.Done():
				return
			}
		}
	}
}
