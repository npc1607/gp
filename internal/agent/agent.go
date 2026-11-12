package agent

import (
	"context"
	"fmt"
	"sync/atomic"

	"gp/internal/ai"
	"gp/internal/core/session"
	"gp/internal/tools"
)

// Status Agent 生命周期状态机
type Status string

const (
	StatusIdle            Status = "idle"
	StatusPrompting       Status = "prompt_processing"
	StatusStreaming       Status = "streaming_model_response"
	StatusToolExecuting   Status = "tool_executing"
	StatusAwaitingApproval Status = "awaiting_approval"
	StatusCompacting      Status = "compacting"
	StatusSettled         Status = "settled"
	StatusDisposed        Status = "disposed"
)

// AgentInstance 单个 Agent 实例 (对齐 Pi 的 AgentSession)
type AgentInstance struct {
	id          string
	roleName    string
	status      atomic.Pointer[Status]
	sessionTree *session.SessionTree
	toolReg     *tools.Registry
	provider    ai.Provider
	modelName   string
}

func NewAgent(id, roleName string, provider ai.Provider, modelName string, reg *tools.Registry) *AgentInstance {
	initialStatus := StatusIdle
	a := &AgentInstance{
		id:          id,
		roleName:    roleName,
		sessionTree: session.NewInMemorySession(id),
		toolReg:     reg,
		provider:    provider,
		modelName:   modelName,
	}
	a.status.Store(&initialStatus)
	return a
}

func (a *AgentInstance) ID() string       { return a.id }
func (a *AgentInstance) RoleName() string { return a.roleName }
func (a *AgentInstance) GetStatus() Status {
	if s := a.status.Load(); s != nil {
		return *s
	}
	return StatusIdle
}

func (a *AgentInstance) SetStatus(next Status) {
	a.status.Store(&next)
}

// Prompt 驱动 Agent 执行一轮任务
func (a *AgentInstance) Prompt(ctx context.Context, userInput string) (string, error) {
	a.SetStatus(StatusPrompting)
	a.sessionTree.AppendMessage(ai.Message{
		Role:    ai.RoleUser,
		Content: userInput,
	})

	// 占位执行逻辑，后续对接 AgentLoop 迭代推理
	a.SetStatus(StatusSettled)
	return fmt.Sprintf("[%s 处理完毕]: 已接收指令: %s", a.roleName, userInput), nil
}
