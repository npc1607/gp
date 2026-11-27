package tui

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// submitUserPrompt 向输入总线提交用户输入：空闲时立即派发，忙碌时进入待处理队列
func (m *MainUIModel) submitUserPrompt(prompt string) {
	if m.primaryBusy && m.inputBus != nil {
		item, qLen := m.inputBus.Enqueue(prompt)
		slog.Debug("enqueue", "component", "INPUT_BUS", "id", item.ID, "qlen", qLen, "text", ClipToWidth(prompt, 60, "…"))
		m.resizeAllBuffers()
		preview := ClipToWidth(strings.ReplaceAll(strings.TrimSpace(prompt), "\n", " "), 48, "…")
		m.addSystemNotice(fmt.Sprintf("⏳ Queued (#%d): %s", qLen, preview), false)
		return
	}
	m.dispatchPrompt(prompt)
}

func (m *MainUIModel) dispatchPrompt(prompt string) {
	slog.Debug("dispatch", "component", "INPUT_BUS", "text", ClipToWidth(prompt, 60, "…"))
	m.resetSubagentViews()
	m.primaryBusy = true
	m.interruptedThisTurn = false
	if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
		activeBuf.RawTextBuffer += "\n[User]: " + prompt + "\n\n"
		activeBuf.Transcript.AddUserMessage(prompt, time.Now())
		activeBuf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), true)
	}
	if m.onUserSubmit != nil {
		m.onUserSubmit(prompt)
	} else {
		m.primaryBusy = false
	}
}

func (m *MainUIModel) rollbackQueuedPrompt() {
	if m.inputBus == nil {
		return
	}
	item, ok := m.inputBus.RollbackLast()
	if !ok {
		return
	}
	slog.Debug("rollback", "component", "INPUT_BUS", "id", item.ID, "remaining", m.inputBus.Len())
	cur := strings.TrimSpace(m.inputArea.Value())
	if cur == "" {
		m.inputArea.SetValue(item.Text)
	} else {
		m.inputArea.SetValue(item.Text + " " + cur)
	}
	m.inputArea.CursorEnd()
	m.resizeAllBuffers()
}

func (m *MainUIModel) renderQueueBar(snap []QueuedPrompt, width int) string {
	latest := strings.ReplaceAll(strings.TrimSpace(snap[len(snap)-1].Text), "\n", " ")
	rollbackLabel := "Alt/Cmd+↑"
	squeezeLabel := "Ctrl+E"
	if m.keymap != nil {
		rollbackLabel = m.keymap.HintLabel("rollback_queue", rollbackLabel)
		squeezeLabel = m.keymap.HintLabel("squeeze_queue", squeezeLabel)
	}
	hints := rollbackLabel + " rollback"
	if len(snap) > 1 {
		hints += " · " + squeezeLabel + " squeeze"
	}
	prefix := m.theme.BoldFg(m.theme.Warning, fmt.Sprintf("⏳ Queued (%d):", len(snap)))
	suffix := m.theme.Fg(m.theme.Dim, " ["+hints+"]")
	avail := max(width-VisibleWidth(prefix)-VisibleWidth(suffix)-2, 12)
	preview := ClipToWidth(latest, avail, "…")
	return ClipToWidth(prefix+" "+m.theme.Fg(m.theme.Muted, preview)+suffix, width, "…")
}
