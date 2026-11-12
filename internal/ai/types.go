package ai

import (
	"context"
	"encoding/json"
	"errors"
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
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function FunctionCall `json:"function"`
}

// FunctionCall 工具调用具体参数
type FunctionCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// TokenUsage Token 消耗明细
type TokenUsage struct {
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	TotalTokens      int64 `json:"total_tokens"`
}

// Message 通用消息结构体
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	Thinking   string     `json:"thinking,omitempty"` // 针对具备显式思考过程的模型 (如 Claude 3.7 / DeepSeek R1)
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"` // 当 Role == RoleTool 时关联的调用 ID
	Timestamp  time.Time  `json:"timestamp"`
}

type messageAux struct {
	Role             Role       `json:"role"`
	Content          string     `json:"content"`
	Thinking         string     `json:"thinking,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	Timestamp        time.Time  `json:"timestamp"`
}

// UnmarshalJSON 兼容解析 thinking 与 reasoning_content 字段 (DeepSeek / OpenAI 规范)
func (m *Message) UnmarshalJSON(data []byte) error {
	var aux messageAux
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	m.Role = aux.Role
	m.Content = aux.Content
	if aux.Thinking != "" {
		m.Thinking = aux.Thinking
	} else if aux.ReasoningContent != "" {
		m.Thinking = aux.ReasoningContent
	} else {
		m.Thinking = ""
	}
	m.ToolCalls = aux.ToolCalls
	m.ToolCallID = aux.ToolCallID
	m.Timestamp = aux.Timestamp
	return nil
}

// NewUserMessage 构造用户输入消息
func NewUserMessage(content string) Message {
	return Message{
		Role:      RoleUser,
		Content:   content,
		Timestamp: time.Now(),
	}
}

// NewAssistantMessage 构造模型响应消息
func NewAssistantMessage(content, thinking string) Message {
	return Message{
		Role:      RoleAssistant,
		Content:   content,
		Thinking:  thinking,
		Timestamp: time.Now(),
	}
}

// NewSystemMessage 构造系统提示词消息
func NewSystemMessage(content string) Message {
	return Message{
		Role:      RoleSystem,
		Content:   content,
		Timestamp: time.Now(),
	}
}

// NewToolMessage 构造工具执行返回结果消息
func NewToolMessage(toolCallID, content string) Message {
	return Message{
		Role:       RoleTool,
		Content:    content,
		ToolCallID: toolCallID,
		Timestamp:  time.Now(),
	}
}

// StreamChunk 流式输出片段
type StreamChunk struct {
	DeltaText     string      `json:"delta_text,omitempty"`
	ThinkingDelta string      `json:"thinking_delta,omitempty"`
	ToolCallDelta *ToolCall   `json:"tool_call_delta,omitempty"`
	FinishReason  string      `json:"finish_reason,omitempty"`
	Usage         *TokenUsage `json:"usage,omitempty"`
	Error         error       `json:"error,omitempty"`
}

type streamChunkAux struct {
	DeltaText     string      `json:"delta_text,omitempty"`
	ThinkingDelta string      `json:"thinking_delta,omitempty"`
	ToolCallDelta *ToolCall   `json:"tool_call_delta,omitempty"`
	FinishReason  string      `json:"finish_reason,omitempty"`
	Usage         *TokenUsage `json:"usage,omitempty"`
	Error         string      `json:"error,omitempty"`
}

// MarshalJSON 自定义序列化，将 error 接口转为字符串输出
func (c StreamChunk) MarshalJSON() ([]byte, error) {
	var errStr string
	if c.Error != nil {
		errStr = c.Error.Error()
	}
	return json.Marshal(streamChunkAux{
		DeltaText:     c.DeltaText,
		ThinkingDelta: c.ThinkingDelta,
		ToolCallDelta: c.ToolCallDelta,
		FinishReason:  c.FinishReason,
		Usage:         c.Usage,
		Error:         errStr,
	})
}

// UnmarshalJSON 自定义反序列化，还原 error 接口
func (c *StreamChunk) UnmarshalJSON(data []byte) error {
	var aux streamChunkAux
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	c.DeltaText = aux.DeltaText
	c.ThinkingDelta = aux.ThinkingDelta
	c.ToolCallDelta = aux.ToolCallDelta
	c.FinishReason = aux.FinishReason
	c.Usage = aux.Usage
	if aux.Error != "" {
		c.Error = errors.New(aux.Error)
	} else {
		c.Error = nil
	}
	return nil
}

// ChatRequest 发送给模型的标准请求
type ChatRequest struct {
	Model         string            `json:"model"`
	Messages      []Message         `json:"messages"`
	Tools         []json.RawMessage `json:"tools,omitempty"`
	ThinkingLevel string            `json:"thinking_level,omitempty"` // "off", "low", "medium", "high"
	Temperature   *float32          `json:"temperature,omitempty"`
	MaxTokens     *int              `json:"max_tokens,omitempty"`
}

// Provider 大模型厂商统一抽象契约 (对应 Pi 的 @earendil-works/pi-ai)
type Provider interface {
	Name() string
	// StreamChat 发起流式推理，返回只读的 StreamChunk 管道
	StreamChat(ctx context.Context, req *ChatRequest) (<-chan StreamChunk, error)
}
