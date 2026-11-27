package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gp/internal/ai"
	"gp/internal/core/compaction"
	"gp/internal/core/retrieval"
	"gp/internal/core/session"
	"gp/internal/core/storage"
	"gp/internal/core/task"
	"gp/internal/tools"
)

// EventType 结构化 Agent 生命周期事件类型
type EventType string

const (
	EventTurnStart      EventType = "turn_start"
	EventTurnEnd        EventType = "turn_end"
	EventToolCallStart  EventType = "tool_call_start"
	EventToolProgress   EventType = "tool_progress"
	EventToolCallEnd    EventType = "tool_call_end"
	EventUsage          EventType = "usage"
	EventContextMetrics EventType = "context_metrics"
)

// AgentEvent 结构化 Agent 运行事件，供 TUI / 日志等消费
type AgentEvent struct {
	Type          EventType
	AgentID       string
	Turn          int
	CallID        string
	ToolName      string
	Args          json.RawMessage
	Result        string
	IsError       bool
	Stalled       bool
	Duration      time.Duration
	Usage         *ai.TokenUsage
	ContextMetric *storage.ContextMetric
	Outcome       string
	Timestamp     time.Time
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
	taskTree             *task.OwnershipTree
	systemPrompt           string
	thinkingLevel          string
	contextWindow          int64
	autoCompactThreshold   float64
	observationMaskWindow  int
	observationMaskEpoch   int
	channelID              string
	contextLedger          *compaction.ContextLedger
	lastPersistedLedgerVer int

	workingSetEnabled     bool
	workingSetBudgetRatio float64
	workingSetMaxTokens   int
	workingSetFloorRatio  float64
	lastToolsTokens       int64

	lastWSInitialized     bool
	lastWSEpoch           int
	lastWSAnchorID        string
	lastWSUserEntryID     string
	lastWSText            string
	lastWSTokens          int64
	lastWSEntries         int
	lastWSMatchedEntities int

	retrievalMu  sync.Mutex
	retrievalIdx *retrieval.Index
}

func NewAgent(id, roleName string, provider ai.Provider, modelName string, reg *tools.Registry) *AgentInstance {
	initialStatus := StatusIdle
	a := &AgentInstance{
		id:                     id,
		roleName:               roleName,
		sessionTree:            session.NewInMemorySession(id),
		toolReg:                reg,
		provider:               provider,
		modelName:              modelName,
		maxTurns:               500,
		contextWindow:          compaction.ResolveContextWindow(modelName),
		autoCompactThreshold:   0.80,
		observationMaskWindow:  defaultObservationMaskWindow,
		observationMaskEpoch:   defaultObservationMaskEpoch,
		contextLedger:          compaction.NewContextLedger(),
		lastPersistedLedgerVer: -1,
		workingSetEnabled:      true,
		workingSetBudgetRatio:  defaultWorkingSetBudgetRatio,
		workingSetMaxTokens:    defaultWorkingSetMaxTokens,
		workingSetFloorRatio:   defaultWorkingSetFloorRatio,
		lastWSEpoch:            -1,
	}
	if reg != nil && len(reg.List()) > 0 {
		for _, rt := range []tools.Tool{
			&restoreObservationTool{agent: a},
			&recallEntriesTool{agent: a},
			&searchContextTool{agent: a},
		} {
			if _, exists := reg.Get(rt.Name()); !exists {
				_ = reg.Register(rt)
			}
		}
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
		a.contextWindow = compaction.ResolveContextWindow(m)
	}
}

// SetThinkingLevel 设置模型推理思考深度 ("off" | "low" | "medium" | "high")
func (a *AgentInstance) SetThinkingLevel(level string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.thinkingLevel = level
}

// GetThinkingLevel 获取当前模型推理思考深度
func (a *AgentInstance) GetThinkingLevel() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.thinkingLevel
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

// SetChannelID 设置当前 Agent 所属渠道标识（如 feishu chat_id / cli / rpc），用于可观测性指标落盘。
func (a *AgentInstance) SetChannelID(channelID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.channelID = strings.TrimSpace(channelID)
}

// GetChannelID 返回当前 Agent 所属渠道标识。
func (a *AgentInstance) GetChannelID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.channelID
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
	a.store = s
	if s != nil && a.taskTree == nil {
		a.taskTree = task.NewOwnershipTree(s)
	}
	a.mu.Unlock()
	a.loadPersistedContextLedger()
	a.loadPersistedWorkingSet()
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
	a.sessionTree = st
	a.contextLedger = compaction.NewContextLedger()
	a.lastPersistedLedgerVer = -1
	a.lastWSInitialized = false
	a.lastWSEpoch = -1
	a.lastWSAnchorID = ""
	a.lastWSUserEntryID = ""
	a.lastWSText = ""
	a.lastWSTokens = 0
	a.lastWSEntries = 0
	a.lastWSMatchedEntities = 0
	a.mu.Unlock()

	a.retrievalMu.Lock()
	a.retrievalIdx = nil
	a.retrievalMu.Unlock()

	a.loadPersistedContextLedger()
	a.loadPersistedWorkingSet()
}

// GetContextLedger 返回当前会话 L1 ContextLedger 的只读拷贝。
func (a *AgentInstance) GetContextLedger() *compaction.ContextLedger {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.contextLedger.Clone()
}

// SetContextLedger 显式覆盖当前会话的 L1 ContextLedger。
func (a *AgentInstance) SetContextLedger(l *compaction.ContextLedger) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.contextLedger = l.Clone()
}

// Prompt 驱动 Agent 执行一轮任务，依托 ReAct 状态循环推进
func (a *AgentInstance) Prompt(ctx context.Context, userInput string) (string, error) {
	return RunLoop(ctx, a, userInput)
}
