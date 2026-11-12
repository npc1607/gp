package tui

import (
	"time"

	"github.com/charmbracelet/bubbles/viewport"
)

// AgentViewBuffer 单个 Agent 专属的虚拟屏幕缓冲
type AgentViewBuffer struct {
	AgentID       string
	RoleName      string
	Status        string // RUNNING, DONE, FAILED, BLOCKED, REPAIR, APPROVAL
	ActiveTool    string
	RawTextBuffer string
	Viewport      viewport.Model
	StartTime     time.Time
	ElapsedTime   time.Duration
}

// NewAgentViewBuffer 创建指定尺寸的 Agent 专属视口与文本缓冲区
func NewAgentViewBuffer(agentID, roleName string, width, height int) *AgentViewBuffer {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	vp := viewport.New(width, height)
	return &AgentViewBuffer{
		AgentID:     agentID,
		RoleName:    roleName,
		Status:      "RUNNING",
		Viewport:    vp,
		StartTime:   time.Now(),
		ElapsedTime: 0,
	}
}

// AppendDelta 追加流式文本增量到 RawTextBuffer，并同步刷新视口内容且滚动至底部
func (b *AgentViewBuffer) AppendDelta(delta string) {
	b.RawTextBuffer += delta
	b.Viewport.SetContent(b.RawTextBuffer)
	b.Viewport.GotoBottom()
	if !b.StartTime.IsZero() {
		b.ElapsedTime = time.Since(b.StartTime)
	}
}

// SetStatus 更新状态与当前活跃工具名称
func (b *AgentViewBuffer) SetStatus(status, tool string) {
	if status != "" {
		b.Status = status
	}
	b.ActiveTool = tool
	if !b.StartTime.IsZero() {
		b.ElapsedTime = time.Since(b.StartTime)
	}
}

// Resize 调整视口尺寸
func (b *AgentViewBuffer) Resize(width, height int) {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	b.Viewport.Width = width
	b.Viewport.Height = height
	b.Viewport.SetContent(b.RawTextBuffer)
}

// Scroll 向上或向下滚动视口（正数为向下，负数为向上）
func (b *AgentViewBuffer) Scroll(delta int) {
	if delta > 0 {
		b.Viewport.LineDown(delta)
	} else if delta < 0 {
		b.Viewport.LineUp(-delta)
	}
}
