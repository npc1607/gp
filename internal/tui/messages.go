package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TickMsg 30 FPS (33ms) 节流渲染时钟消息
type TickMsg time.Time

// AgentStreamDeltaMsg 某个 Agent 的流式 Token 差量
type AgentStreamDeltaMsg struct {
	AgentID   string
	DeltaText string
}

// AgentStatusChangedMsg Agent 状态跃迁通知 (状态机变化、工具调用)
type AgentStatusChangedMsg struct {
	AgentID   string
	RoleName  string
	NewStatus string // RUNNING, DONE, FAILED, BLOCKED, REPAIR, APPROVAL
	ToolName  string
}

// DAGNodeStatusMsg DAG 拓扑节点状态变更消息
type DAGNodeStatusMsg struct {
	NodeID      string
	Role        string
	Status      string // RUNNING, BLOCKED, REPAIR, DONE, APPROVAL, FAILED
	Attempt     int
	MaxAttempts int
	Output      string
	ErrorMsg    string
}

// ActiveCoroutineMsg 动态任务泳道协程生命周期变更
type ActiveCoroutineMsg struct {
	AgentID  string
	Role     string
	TaskDesc string
	Elapsed  time.Duration
	Active   bool // true: 活跃/更新; false: 结束退出
}

// UserSubmitPromptMsg 用户在输入框中提交的提示词
type UserSubmitPromptMsg struct {
	Prompt string
}

// ErrorMsg 错误通知消息
type ErrorMsg struct {
	Err error
}

// tickEvery creates a tea.Cmd that dispatches a TickMsg after the given duration.
func tickEvery(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return TickMsg(t)
	})
}
