// Package agp 定义 Agent Gateway Protocol（AGP）的线上协议与客户端实现。
//
// 协议本身是逐行 JSON（JSONL）的双向帧流：调用方写入 Request 帧，Agent 侧回写
// Event 帧。除了 gp 内置的 RPC 服务端（internal/modes/rpc）之外，任何前端
// （CLI、IDE 插件、IM 机器人适配器等）都可以通过本包作为客户端接入，而无需
// 依赖服务端实现或 agent 内部结构。
package agp

const (
	OpHello         = "hello"
	OpRun           = "run"
	OpResetSession  = "reset_session"
	OpAbort         = "abort"
	OpPing          = "ping"
	OpApprovalReply = "approval_reply"
	OpQuestionReply = "question_reply"
	OpListSessions  = "list_sessions"
	OpResumeSession = "resume_session"
	OpDeleteSession = "delete_session"
	OpContextStats  = "ctx_stat"
)

const (
	EventHelloAck        = "hello_ack"
	EventStarted         = "started"
	EventThinking        = "thinking"
	EventToolStart       = "tool_start"
	EventToolProgress    = "tool_progress"
	EventToolEnd         = "tool_end"
	EventSubagentStart   = "subagent_start"
	EventSubagentEnd     = "subagent_end"
	EventApprovalRequest = "approval_request"
	EventQuestionRequest = "question_request"
	EventDelta           = "delta"
	EventFinal           = "final"
	EventResetAck        = "reset_ack"
	EventAbortAck        = "abort_ack"
	EventSessionsList    = "sessions_list"
	EventResumeAck       = "resume_ack"
	EventDeleteAck       = "delete_ack"
	EventContextStats    = "ctx_stat"
	EventPong            = "pong"
	EventError           = "error"
)

// CapabilityRule defines an allowlisted external capability (skill, mcp, js, cli, etc.).
type CapabilityRule struct {
	Kind  string   `json:"kind"`
	Name  string   `json:"name"`
	Allow []string `json:"allow,omitempty"`
	Paths []string `json:"paths,omitempty"`
}

// ExecutionPolicy carries structured workspace, tool, capability, and path boundaries for an agent run.
type ExecutionPolicy struct {
	WorkDir      string           `json:"work_dir,omitempty"`
	Tools        []string         `json:"tools,omitempty"`
	AllowedDirs  []string         `json:"allowed_dirs,omitempty"`
	DeniedPaths  []string         `json:"denied_paths,omitempty"`
	AllowBash    *bool            `json:"allow_bash,omitempty"`
	AllowWrite   *bool            `json:"allow_write,omitempty"`
	BashMode     string           `json:"bash_mode,omitempty"`
	Capabilities []CapabilityRule `json:"capabilities,omitempty"`
}

// CallerContext carries the human initiator identity and approval capability.
type CallerContext struct {
	SenderID      string `json:"sender_id,omitempty"`
	IsBoss        bool   `json:"is_boss,omitempty"`
	AllowApproval bool   `json:"allow_approval,omitempty"`
}

// RunOptions carries per-invocation overrides for an agent run.
type RunOptions struct {
	WorkDir    string           `json:"work_dir,omitempty"`
	Model      string           `json:"model,omitempty"`
	Tools      []string         `json:"tools,omitempty"`
	TimeoutSec int              `json:"timeout_sec,omitempty"`
	Policy     *ExecutionPolicy `json:"policy,omitempty"`
	Caller     *CallerContext   `json:"caller,omitempty"`
}

// Request represents an incoming AGP JSONL command frame.
type Request struct {
	ID         string            `json:"id"`
	Op         string            `json:"op"`
	Client     string            `json:"client,omitempty"`
	SessionID  string            `json:"session_id,omitempty"`
	Prompt     string            `json:"prompt,omitempty"`
	TargetID   string            `json:"target_id,omitempty"`
	ApprovalID string            `json:"approval_id,omitempty"`
	Approved   bool              `json:"approved,omitempty"`
	QuestionID string            `json:"question_id,omitempty"`
	Answer     string            `json:"answer,omitempty"`
	Skipped    bool              `json:"skipped,omitempty"`
	Options    *RunOptions       `json:"options,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// Usage reports token usage for a completed turn.
type Usage struct {
	InputTokens      int `json:"input_tokens,omitempty"`
	OutputTokens     int `json:"output_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// SessionSummary describes a persisted historical session returned by list_sessions.
type SessionSummary struct {
	SessionID    string `json:"session_id"`
	MessageCount int    `json:"message_count"`
	FirstPrompt  string `json:"first_prompt,omitempty"`
	UpdatedAt    string `json:"updated_at"`
}

// ContextConstraintSummary summarizes one ContextLedger entry for observability.
type ContextConstraintSummary struct {
	ID           string   `json:"id"`
	Category     string   `json:"category"`
	Statement    string   `json:"statement"`
	Status       string   `json:"status"`
	SupersededBy string   `json:"superseded_by,omitempty"`
	Provenance   []string `json:"provenance,omitempty"`
}

// ContextTurnMetric mirrors one persisted context_metrics turn row.
type ContextTurnMetric struct {
	TurnIndex        int     `json:"turn_index"`
	StrategyMode     string  `json:"strategy_mode"`
	EpochID          int     `json:"epoch_id"`
	EpochMaskedCount int     `json:"epoch_masked_count"`
	LedgerVersion    int     `json:"ledger_version"`
	LedgerActiveCnt  int     `json:"ledger_active_cnt"`
	LedgerSuperseded int     `json:"ledger_superseded"`
	LedgerHash       string  `json:"ledger_hash"`
	ContextBuildMs   float64 `json:"context_build_ms"`
	InputTokens      int     `json:"input_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	RecallCalls      int     `json:"recall_calls"`
	RestoreCalls     int     `json:"restore_calls"`
	CreatedAt        string  `json:"created_at,omitempty"`
}

// ContextStats carries read-only session context/cache observability data returned by ctx_stat.
type ContextStats struct {
	SessionID             string                     `json:"session_id"`
	MessageCount          int                        `json:"message_count"`
	EpochID               int                        `json:"epoch_id"`
	EpochStride           int                        `json:"epoch_stride"`
	EpochPosition         int                        `json:"epoch_position"`
	FreezeBoundary        int                        `json:"freeze_boundary"`
	MaskedCount           int                        `json:"masked_count"`
	MaskWindow            int                        `json:"mask_window"`
	LedgerVersion         int                        `json:"ledger_version"`
	LedgerHash            string                     `json:"ledger_hash,omitempty"`
	LedgerActiveCnt       int                        `json:"ledger_active_cnt"`
	LedgerSuperseded      int                        `json:"ledger_superseded"`
	LedgerResolvedCnt     int                        `json:"ledger_resolved_cnt"`
	ActiveConstraints     []ContextConstraintSummary `json:"active_constraints,omitempty"`
	SupersededConstraints []ContextConstraintSummary `json:"superseded_constraints,omitempty"`
	RecentTurns           []ContextTurnMetric        `json:"recent_turns,omitempty"`
	TotalRecallCalls      int                        `json:"total_recall_calls"`
	TotalRestoreCalls     int                        `json:"total_restore_calls"`
	TotalTurns            int                        `json:"total_turns"`
}

// Event represents an outgoing AGP JSONL event frame.
type Event struct {
	ID           string           `json:"id"`
	Event        string           `json:"event"`
	SessionID    string           `json:"session_id,omitempty"`
	Sessions     []SessionSummary `json:"sessions,omitempty"`
	ContextStats *ContextStats    `json:"context_stats,omitempty"`
	ApprovalID   string         `json:"approval_id,omitempty"`
	QuestionID   string         `json:"question_id,omitempty"`
	Question     string         `json:"question,omitempty"`
	Options      []string       `json:"options,omitempty"`
	Text         string         `json:"text,omitempty"`
	Tool         string         `json:"tool,omitempty"`
	Args         map[string]any `json:"args,omitempty"`
	Summary      string         `json:"summary,omitempty"`
	Code         string         `json:"code,omitempty"`
	Message      string         `json:"message,omitempty"`
	AgentName    string         `json:"agent_name,omitempty"`
	AgentVersion string         `json:"agent_version,omitempty"`
	Stalled      bool           `json:"stalled,omitempty"`
	Usage        *Usage         `json:"usage,omitempty"`
}

// Terminal 判断该事件是否标志一次运行的结束（不会再有后续事件）。
func (e Event) Terminal() bool {
	return e.Event == EventFinal || e.Event == EventError
}
