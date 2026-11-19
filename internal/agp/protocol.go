// Package agp 定义 Agent Gateway Protocol（AGP）的线上协议与客户端实现。
//
// 协议本身是逐行 JSON（JSONL）的双向帧流：调用方写入 Request 帧，Agent 侧回写
// Event 帧。除了 gp 内置的 RPC 服务端（internal/modes/rpc）之外，任何前端
// （CLI、IDE 插件、IM 机器人适配器等）都可以通过本包作为客户端接入，而无需
// 依赖服务端实现或 agent 内部结构。
package agp

const (
	OpHello        = "hello"
	OpRun          = "run"
	OpResetSession = "reset_session"
	OpAbort        = "abort"
	OpPing         = "ping"
)

const (
	EventHelloAck      = "hello_ack"
	EventStarted       = "started"
	EventThinking      = "thinking"
	EventToolStart     = "tool_start"
	EventToolEnd       = "tool_end"
	EventSubagentStart = "subagent_start"
	EventSubagentEnd   = "subagent_end"
	EventDelta         = "delta"
	EventFinal         = "final"
	EventResetAck      = "reset_ack"
	EventAbortAck      = "abort_ack"
	EventPong          = "pong"
	EventError         = "error"
)

// RunOptions carries per-invocation overrides for an agent run.
type RunOptions struct {
	WorkDir    string   `json:"work_dir,omitempty"`
	Model      string   `json:"model,omitempty"`
	Tools      []string `json:"tools,omitempty"`
	TimeoutSec int      `json:"timeout_sec,omitempty"`
}

// Request represents an incoming AGP JSONL command frame.
type Request struct {
	ID        string            `json:"id"`
	Op        string            `json:"op"`
	Client    string            `json:"client,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	Prompt    string            `json:"prompt,omitempty"`
	TargetID  string            `json:"target_id,omitempty"`
	Options   *RunOptions       `json:"options,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Usage reports token usage for a completed turn.
type Usage struct {
	InputTokens     int `json:"input_tokens,omitempty"`
	OutputTokens    int `json:"output_tokens,omitempty"`
	CacheReadTokens int `json:"cache_read_tokens,omitempty"`
}

// Event represents an outgoing AGP JSONL event frame.
type Event struct {
	ID           string         `json:"id"`
	Event        string         `json:"event"`
	SessionID    string         `json:"session_id,omitempty"`
	Text         string         `json:"text,omitempty"`
	Tool         string         `json:"tool,omitempty"`
	Args         map[string]any `json:"args,omitempty"`
	Summary      string         `json:"summary,omitempty"`
	Code         string         `json:"code,omitempty"`
	Message      string         `json:"message,omitempty"`
	AgentName    string         `json:"agent_name,omitempty"`
	AgentVersion string         `json:"agent_version,omitempty"`
	Usage        *Usage         `json:"usage,omitempty"`
}

// Terminal 判断该事件是否标志一次运行的结束（不会再有后续事件）。
func (e Event) Terminal() bool {
	return e.Event == EventFinal || e.Event == EventError
}
