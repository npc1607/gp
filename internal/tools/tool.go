package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// ToolContext 工具执行的隔离上下文 (对齐 Pi 的 ToolContext)
// 彻底杜绝全局状态污染：每个 Agent 拥有自己独立的 Cwd 与环境映射
type ToolContext struct {
	context.Context
	AgentID      string
	Cwd          string
	Env          map[string]string
	AllowedPaths []string
	EventChan    chan<- ToolEvent
}

// ToolEvent 工具执行进度事件 (用于 TUI 动态进度展示)
type ToolEvent struct {
	ToolName string
	Action   string // 比如 "reading main.go (1~100)"
	IsError  bool
}

// ToolResult 工具执行输出
type ToolResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error"`
	Details any    `json:"details,omitempty"`
}

// Tool 工具通用契约接口
type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	Execute(ctx *ToolContext, rawArgs json.RawMessage) (*ToolResult, error)
}

// Registry 线程安全的工具注册中心
type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

func (r *Registry) Register(t Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[t.Name()]; exists {
		return fmt.Errorf("tool already registered: %s", t.Name())
	}
	r.tools[t.Name()] = t
	return nil
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		res = append(res, t)
	}
	return res
}

// ScopedRegistry 根据白名单裁剪出针对特定工种的子集工具箱
func (r *Registry) ScopedRegistry(allowlist []string) *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	scoped := NewRegistry()
	for _, name := range allowlist {
		if t, ok := r.tools[name]; ok {
			scoped.tools[name] = t
		}
	}
	return scoped
}
