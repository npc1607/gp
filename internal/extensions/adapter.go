package extensions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// RiskLevel 声明外部动作的安全风险级别
type RiskLevel string

const (
	RiskRead        RiskLevel = "read"
	RiskWrite       RiskLevel = "write"
	RiskDestructive RiskLevel = "destructive"
)

// 标准化错误码（供模型确定性自纠与决策）
const (
	ErrNotFound       = "ERR_NOT_FOUND"
	ErrDisabled       = "ERR_DISABLED"
	ErrInvalidArgs    = "ERR_INVALID_ARGS"
	ErrAuthMissing    = "ERR_AUTH_MISSING"
	ErrDenied         = "ERR_DENIED"
	ErrAdapterOffline = "ERR_ADAPTER_OFFLINE"
	ErrTimeout        = "ERR_TIMEOUT"
	ErrUpstream       = "ERR_UPSTREAM"
)

// ActionDef 描述一个对外可发现、可调用的标准化动作
type ActionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Risk        RiskLevel       `json:"risk"`
	Timeout     time.Duration   `json:"timeout,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	Tags        []string        `json:"tags,omitempty"`
}

// ResolvedAuth 保存已解析的临时鉴权凭证（仅内存传递，严禁序列化落盘）
type ResolvedAuth struct {
	Type       string
	Header     string
	QueryParam string
	Prefix     string
	Token      string
}

// InvokeRequest 传递给底层 Adapter.Invoke 的统一请求对象
type InvokeRequest struct {
	Action     string
	ActionRisk RiskLevel
	Args       map[string]any
	Cwd        string
	AgentID    string
	Auth       ResolvedAuth
	Timeout    time.Duration
}

// InvokeResult 底层 Adapter 执行返回的原始结果
type InvokeResult struct {
	Content     string
	IsError     bool
	ErrorCode   string
	AdapterType string
	Risk        RiskLevel
	Duration    time.Duration
	Truncated   bool
	SpillPath   string
	RawBytes    int
}

// Adapter 统一外部能力传输适配器接口（guide / cli / script / http / openapi / mcp）
type Adapter interface {
	Type() string
	DiscoverActions(ctx context.Context) ([]ActionDef, error)
	Invoke(ctx context.Context, req InvokeRequest) (*InvokeResult, error)
	HealthCheck(ctx context.Context) error
	Close() error
}

// formatCodedError 将错误统一格式化为 `[ERR_CODE] message` 形态
func formatCodedError(code, msg string) string {
	if code == "" {
		code = ErrUpstream
	}
	return fmt.Sprintf("[%s] %s", code, msg)
}
