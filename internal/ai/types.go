package ai

import (
	"context"
	"encoding/json"
	"time"
)

// Role 消息角色
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall 表示模型生成的工具调用请求
type ToolCall struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"` // "function"
	Function  FunctionCall    `json:"function"`
}

// FunctionCall 工具调用具体参数
type FunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// TokenUsage Token 消耗明细
type TokenUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	CacheReadTokens    int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens   int64 `json:"cache_write_tokens,omitempty"`
	TotalTokens        int64 `json:"total_tokens"`
}

// Message 通用消息结构体
type Message struct {
	Role         Role             `json:"role"`
	Content      string           `json:"content"`
	Thinking     string           `json:"thinking,omitempty"` // 针对具备显式思考过程的模型 (如 Claude 3.7 / DeepSeek R1)
	ToolCalls    []ToolCall       `json:"tool_calls,omitempty"`
	ToolCallID   string           `json:"tool_call_id,omitempty"` // 当 Role == RoleTool 时关联的调用 ID
	Timestamp    time.Time        `json:"timestamp"`
}

// StreamChunk 流式输出片段
type StreamChunk struct {
	DeltaText     string       // 增量文本
	ThinkingDelta string       // 增量思考内容
	ToolCallDelta *ToolCall    // 增量工具调用
	FinishReason  string       // "stop", "tool_calls", "length" 等
	Usage         *TokenUsage  // 最终 token 统计 (通常在最后一个 chunk)
	Error         error
}

// ChatRequest 发送给模型的标准请求
type ChatRequest struct {
	Model         string          `json:"model"`
	Messages      []Message       `json:"messages"`
	Tools         []json.RawMessage `json:"tools,omitempty"`
	ThinkingLevel string          `json:"thinking_level,omitempty"` // "off", "low", "medium", "high"
	Temperature   *float32        `json:"temperature,omitempty"`
	MaxTokens     *int            `json:"max_tokens,omitempty"`
}

// Provider 大模型厂商统一抽象契约 (对应 Pi 的 @earendil-works/pi-ai)
type Provider interface {
	Name() string
	// StreamChat 发起流式推理，返回只读的 StreamChunk 管道
	StreamChat(ctx context.Context, req *ChatRequest) (<-chan StreamChunk, error)
}
