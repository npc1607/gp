package agent

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"gp/internal/ai"
	"gp/internal/core/session"
	"gp/internal/core/storage"
	"gp/internal/core/task"
	"gp/internal/tools"
)

// EventType 结构化 Agent 生命周期事件类型
type EventType string

const (
	EventTurnStart     EventType = "turn_start"
	EventTurnEnd       EventType = "turn_end"
	EventToolCallStart EventType = "tool_call_start"
	EventToolCallEnd   EventType = "tool_call_end"
	EventUsage         EventType = "usage"
)

// AgentEvent 结构化 Agent 运行事件，供 TUI / 日志等消费
type AgentEvent struct {
	Type       EventType
	AgentID    string
	Turn       int
	CallID     string
	ToolName   string
	Args       json.RawMessage
	Result     string
	IsError    bool
	Duration   time.Duration
	Usage      *ai.TokenUsage
	Outcome    string
	Timestamp  time.Time
}

// Status Agent 生命周期状态机
type Status string

const (
	StatusIdle             Status = "idle"
	StatusPrompting        Status = "prompt_processing"
	StatusStreaming        Status = "streaming_model_response"
	StatusToolExecuting    Status = "tool_executing"
	StatusAwaitingApproval Status = "awaiting_approval"
	StatusCompacting       Status = "compacting"
	StatusSettled          Status = "settled"
	StatusDisposed         Status = "disposed"
)

// AgentInstance 单个 Agent 实例 (对齐 Pi 的 AgentSession)
type AgentInstance struct {
	id            string
	roleName      string
	status        atomic.Pointer[Status]
	sessionTree   *session.SessionTree
	toolReg       *tools.Registry
	provider      ai.Provider
	modelName     string
	mu            sync.RWMutex
	cwd           string
	maxTurns      int
	streamHandler func(chunk ai.StreamChunk)
	eventHandler  func(ev AgentEvent)
	store         storage.Storage
	taskTree      *task.OwnershipTree
	systemPrompt  string
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
		maxTurns:    500,
	}
	a.status.Store(&initialStatus)
	return a
}

func (a *AgentInstance) ID() string { return a.id }
func (a *AgentInstance) SessionID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.sessionTree != nil && a.sessionTree.SessionID() != "" {
		return a.sessionTree.SessionID()
	}
	return a.id
}
func (a *AgentInstance) RoleName() string { return a.roleName }
func (a *AgentInstance) ModelName() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.modelName
}
func (a *AgentInstance) SetModelName(m string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if m != "" {
		a.modelName = m
	}
}
func (a *AgentInstance) GetProvider() ai.Provider {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.provider
}
func (a *AgentInstance) SetProvider(p ai.Provider) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p != nil {
		a.provider = p
	}
}
func (a *AgentInstance) GetSessionTree() *session.SessionTree { return a.sessionTree }
func (a *AgentInstance) GetRegistry() *tools.Registry         { return a.toolReg }

func (a *AgentInstance) GetStatus() Status {
	if s := a.status.Load(); s != nil {
		return *s
	}
	return StatusIdle
}

func (a *AgentInstance) SetStatus(next Status) {
	a.status.Store(&next)
}

func (a *AgentInstance) SetCwd(cwd string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cwd = cwd
}

func (a *AgentInstance) GetCwd() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cwd
}

func (a *AgentInstance) SetMaxTurns(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.maxTurns = n
}

func (a *AgentInstance) GetMaxTurns() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.maxTurns
}

func (a *AgentInstance) SetStreamHandler(h func(chunk ai.StreamChunk)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.streamHandler = h
}

func (a *AgentInstance) GetStreamHandler() func(chunk ai.StreamChunk) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.streamHandler
}

func (a *AgentInstance) SetEventHandler(h func(ev AgentEvent)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.eventHandler = h
}

func (a *AgentInstance) EmitEvent(ev AgentEvent) {
	a.mu.RLock()
	h := a.eventHandler
	a.mu.RUnlock()
	if h != nil {
		if ev.AgentID == "" {
			ev.AgentID = a.id
		}
		if ev.Timestamp.IsZero() {
			ev.Timestamp = time.Now()
		}
		h(ev)
	}
}

func (a *AgentInstance) SetStorage(s storage.Storage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.store = s
	if s != nil && a.taskTree == nil {
		a.taskTree = task.NewOwnershipTree(s)
	}
}

func (a *AgentInstance) GetStorage() storage.Storage {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.store
}

func (a *AgentInstance) SetTaskTree(tree *task.OwnershipTree) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.taskTree = tree
}

func (a *AgentInstance) GetTaskTree() *task.OwnershipTree {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.taskTree
}

func (a *AgentInstance) SetSystemPrompt(p string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.systemPrompt = p
}

func (a *AgentInstance) GetSystemPrompt() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.systemPrompt
}

func (a *AgentInstance) SetSessionTree(st *session.SessionTree) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionTree = st
}

// Prompt 驱动 Agent 执行一轮任务，依托 ReAct 状态循环推进
func (a *AgentInstance) Prompt(ctx context.Context, userInput string) (string, error) {
	return RunLoop(ctx, a, userInput)
}
