package tui

import (
	"time"

	"github.com/charmbracelet/bubbles/viewport"
)

// AgentViewBuffer 单个 Agent 专属的虚拟屏幕缓冲与结构化 Transcript
type AgentViewBuffer struct {
	AgentID       string
	RoleName      string
	Status        string // RUNNING, DONE, FAILED, BLOCKED, REPAIR, APPROVAL
	ActiveTool    string
	RawTextBuffer string
	Transcript    *Transcript
	Viewport      viewport.Model
	StartTime     time.Time
	ElapsedTime   time.Duration
	Dirty         bool
	AutoScroll    bool
	LastSync      time.Time
}

// NewAgentViewBuffer 创建指定尺寸的 Agent 专属视口与结构化缓冲区
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
		Transcript:  NewTranscript("", ""),
		Viewport:    vp,
		StartTime:   time.Now(),
		ElapsedTime: 0,
	}
}

// SetModelInfo 设置当前 Agent 对应的模型与 Provider 标识（用于渲染 reply 边框）
func (b *AgentViewBuffer) SetModelInfo(model, provider string) {
	b.Transcript.Model = model
	b.Transcript.Provider = provider
	b.Dirty = true
}

// AppendDelta 追加流式文本增量到 Transcript 与 RawTextBuffer
func (b *AgentViewBuffer) AppendDelta(delta string) {
	b.RawTextBuffer += delta
	b.Transcript.AppendTextDelta(delta, DefaultUIConfig())
	b.Dirty = true
	if !b.StartTime.IsZero() {
		b.ElapsedTime = time.Since(b.StartTime)
	}
}

// SyncViewport 使用结构化 Transcript 重新渲染并刷新视口内容
func (b *AgentViewBuffer) SyncViewport(t Theme, cfg UIConfig, spinnerGlyph string, autoScroll bool) {
	wasAtBottom := b.Viewport.AtBottom()
	rendered := b.Transcript.Render(b.Viewport.Width, t, cfg, spinnerGlyph)
	b.Viewport.SetContent(rendered)
	if autoScroll || wasAtBottom {
		b.Viewport.GotoBottom()
	}
	b.Dirty = false
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

// Resize 调整视口尺寸并标记需要重排
func (b *AgentViewBuffer) Resize(width, height int) {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}
	if b.Viewport.Width != width || b.Viewport.Height != height {
		b.Viewport.Width = width
		b.Viewport.Height = height
		b.Dirty = true
	}
}

// Scroll 向上或向下滚动视口（正数为向下，负数为向上）
func (b *AgentViewBuffer) Scroll(delta int) {
	if delta > 0 {
		b.Viewport.LineDown(delta)
	} else if delta < 0 {
		b.Viewport.LineUp(-delta)
	}
}
