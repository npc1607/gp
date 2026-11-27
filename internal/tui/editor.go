package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
)

// WorkingSpinners 对应 pi-zentui 的 5 套工作行动画预设（claude-inspired / star-bloom 采用对称回弹呼吸序列）
var WorkingSpinners = map[string][]string{
	"braille":         {"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	"star-bloom":      {"·", "✦", "✧", "✶", "✧", "✦"},
	"pinwheel":        {"-", "\\", "|", "/"},
	"claude-inspired": {"·", "✢", "✳", "✶", "✻", "✽", "✻", "✶", "✳", "✢"},
	"pulse":           {"⠀⠶⠀", "⠰⣿⠆", "⢾⣉⡷", "⣏⠀⣹", "⡁⠀⢈"},
}

// WorkingLineState 维护编辑器顶框内嵌的运行中状态行
type WorkingLineState struct {
	Active    bool
	Phase     string // "connecting" | "thinking" | "tool" | "writing"
	TaskDesc  string
	ToolName  string
	StartedAt time.Time
	FrameIdx  int
	Tick      int
}

// TickFrame 以 30fps 推进细腻的扫光与呼吸相位，并按 10fps 节奏推进星芒帧
func (w *WorkingLineState) TickFrame() {
	if !w.Active {
		return
	}
	w.Tick++
	if w.Tick%3 == 0 {
		w.FrameIdx++
	}
}

func (w *WorkingLineState) Advance() {
	if w.Active {
		w.FrameIdx++
		w.Tick += 3
	}
}

func (w *WorkingLineState) SpinnerGlyph(preset string) string {
	frames, ok := WorkingSpinners[preset]
	if !ok || len(frames) == 0 {
		frames = WorkingSpinners["claude-inspired"]
	}
	return frames[w.FrameIdx%len(frames)]
}

func (w *WorkingLineState) phaseColors(t Theme) (accent lipgloss.Color, peak lipgloss.Color) {
	switch w.Phase {
	case "thinking":
		return t.Thinking, lipgloss.Color("#fff3d6")
	case "writing":
		return t.Success, t.Text
	case "tool":
		if w.ToolName != "" {
			return t.ToolBorderColor(w.ToolName, false, false), t.Text
		}
		return t.Accent, t.Text
	default:
		return t.Accent, t.Text
	}
}

// BreathGlyph 返回用于 Transcript 内部步骤的克制呼吸指示点（与底部主动画视觉去重）
func (w *WorkingLineState) BreathGlyph(t Theme) string {
	if !w.Active {
		return ""
	}
	accent, _ := w.phaseColors(t)
	return RenderBreathingDot(t.BorderMuted, accent, w.Tick)
}

func (w *WorkingLineState) RenderLabel(t Theme, cfg UIConfig) string {
	if !w.Active {
		return ""
	}
	glyph := w.SpinnerGlyph(cfg.WorkingSpinner)
	desc := w.TaskDesc
	if desc == "" {
		switch w.Phase {
		case "thinking":
			desc = "Thinking…"
		case "tool":
			desc = "Running…"
		case "writing":
			desc = "Writing…"
		default:
			desc = "Connecting…"
		}
	}
	elapsed := time.Since(w.StartedAt)
	if w.StartedAt.IsZero() {
		elapsed = 0
	}

	phaseAccent, peakColor := w.phaseColors(t)
	// 星芒图标做平滑明暗呼吸（暗灰 <-> 阶段主色）
	breath := 0.30 + 0.70*BreathingFactor(w.Tick, 36)
	glyphColor := LerpColor(t.Muted, phaseAccent, breath)
	glyphStyle := lipgloss.NewStyle().Foreground(glyphColor)
	if breath > 0.65 {
		glyphStyle = glyphStyle.Bold(true)
	}

	// 状态词做柔和流光扫掠（底色 -> 高光波峰）
	shimmerDesc := RenderShimmerText(desc, t.Muted, peakColor, w.Tick, true)

	parts := []string{
		glyphStyle.Render(glyph),
		shimmerDesc,
	}
	if w.ToolName != "" {
		parts = append(parts, t.Fg(t.Muted, "("+w.ToolName+")"))
	}
	parts = append(parts, t.Fg(t.Muted, formatShortDuration(elapsed)))
	return strings.Join(parts, " ")
}

// EditorMeta 编辑器底部元数据区（模型、Provider、Thinking 等级、提示等）
type EditorMeta struct {
	ModelName     string
	ProviderName  string
	ThinkingLevel string
	VimBadge      string
	SelectedAll   bool
	RightHint     string
	ShellMode     bool
}

// RenderEditor 渲染 pi-zentui 风格的编辑器（Opencode / Accent-Rail / Minimalist）
func RenderEditor(
	input textinput.Model,
	width int,
	t Theme,
	cfg UIConfig,
	meta EditorMeta,
	working *WorkingLineState,
) string {
	if width < 20 {
		width = 20
	}

	accentColor := t.Accent
	if meta.ShellMode {
		accentColor = t.SyntaxOperator
	}
	rail := t.BoldFg(accentColor, "│") + " "
	railW := 2
	innerW := width - railW

	// 顶部边框（若运行中则嵌入 WorkingLine）
	topBorder := t.Fg(t.BorderMuted, strings.Repeat("─", width))
	if working != nil && working.Active {
		wLabel := working.RenderLabel(t, cfg)
		if wLabel != "" && width >= 24 {
			topBorder = LabeledRule(width, t, t.BorderMuted, wLabel, "left", 2)
		}
	}
	botBorder := t.Fg(t.BorderMuted, strings.Repeat("─", width))

	// 输入行内容（直接使用 textinput.View()，完整支持左右移动光标、Ctrl+W/Alt+Backspace 删词及长文本水平滚动）
	input.Width = max(innerW-2, 8)
	inputLine := ClipToWidth(input.View(), innerW, "")
	if meta.SelectedAll && input.Value() != "" {
		selText := ClipToWidth(input.Value(), max(innerW-2, 8), "…")
		inputLine = lipgloss.NewStyle().Reverse(true).Render(selText)
	}

	// 元数据行: <provider-icon> <model>  <thinking>        <rightHint>
	pIcon := ProviderIcon(meta.ProviderName, meta.ModelName, cfg.Icons)
	modelLabel := meta.ModelName
	if modelLabel == "" {
		modelLabel = "model"
	}
	var leftMeta []string
	if pIcon != "" {
		leftMeta = append(leftMeta, t.Fg(t.Accent, pIcon))
	}
	leftMeta = append(leftMeta, t.BoldFg(t.Text, modelLabel))
	if meta.ProviderName != "" {
		leftMeta = append(leftMeta, t.Fg(t.Muted, meta.ProviderName))
	}
	thinkLevel := strings.TrimSpace(meta.ThinkingLevel)
	if thinkLevel == "" {
		thinkLevel = "off"
	}
	if thinkLevel == "off" {
		leftMeta = append(leftMeta, t.Fg(t.Muted, "think:"+thinkLevel))
	} else {
		leftMeta = append(leftMeta, t.Fg(t.Warning, "think:"+thinkLevel))
	}
	if meta.VimBadge != "" {
		if meta.VimBadge == "NORMAL" {
			leftMeta = append(leftMeta, t.BoldFg(t.Accent, "-- NORMAL --"))
		} else {
			leftMeta = append(leftMeta, t.Fg(t.Success, "-- INSERT --"))
		}
	}
	if meta.SelectedAll {
		leftMeta = append(leftMeta, t.BoldFg(t.Warning, "[ALL]"))
	}
	metaLeft := strings.Join(leftMeta, " ")
	metaRight := ""
	if meta.RightHint != "" {
		metaRight = t.Fg(t.Muted, meta.RightHint)
	}
	metaLine := composeLeftRight(metaLeft, metaRight, innerW)

	switch cfg.EditorStyle {
	case "accent-rail":
		return strings.Join([]string{
			rail + PadToWidth(inputLine, innerW),
			rail + PadToWidth(metaLine, innerW),
		}, "\n")

	case "minimalist":
		top := LabeledTopBorder(width, t, t.BorderMuted, metaLeft, "left", 1)
		bot := t.Fg(t.BorderMuted, "╰"+strings.Repeat("─", width-2)+"╯")
		bar := t.Fg(t.BorderMuted, "│")
		row := bar + PadToWidth(" "+inputLine, width-2) + bar
		return strings.Join([]string{top, row, bot}, "\n")

	default: // "opencode" (pi-zentui 默认)
		emptyRail := rail + strings.Repeat(" ", innerW)
		return strings.Join([]string{
			topBorder,
			emptyRail,
			rail + PadToWidth(inputLine, innerW),
			emptyRail,
			rail + PadToWidth(metaLine, innerW),
			botBorder,
		}, "\n")
	}
}

// ZoomedEditorHeight 返回放大多行编辑器占用的总行数
const ZoomedEditorHeight = 12

// RenderZoomedEditor 渲染放大多行编辑器（Ctrl+X 切换，Alt+Enter / Ctrl+J 提交，Enter 换行）
func RenderZoomedEditor(
	area textarea.Model,
	width int,
	t Theme,
	cfg UIConfig,
	meta EditorMeta,
	working *WorkingLineState,
) string {
	if width < 24 {
		width = 24
	}
	rail := t.BoldFg(t.Accent, "│") + " "
	railW := 2
	innerW := width - railW

	topBorder := LabeledRule(width, t, t.Accent, t.BoldFg(t.Accent, "Zoomed Editor (Alt+Enter send · Ctrl+X compact)"), "left", 2)
	if working != nil && working.Active {
		if wLabel := working.RenderLabel(t, cfg); wLabel != "" {
			topBorder = LabeledRule(width, t, t.BorderMuted, wLabel, "left", 2)
		}
	}
	botBorder := t.Fg(t.BorderMuted, strings.Repeat("─", width))

	area.SetWidth(max(innerW-2, 12))
	area.SetHeight(ZoomedEditorHeight - 3)
	var rawLines []string
	if meta.SelectedAll && area.Value() != "" {
		selStyle := lipgloss.NewStyle().Reverse(true)
		for l := range strings.SplitSeq(area.Value(), "\n") {
			clipped := ClipToWidth(l, max(innerW-2, 8), "…")
			if clipped != "" {
				rawLines = append(rawLines, selStyle.Render(clipped))
			} else {
				rawLines = append(rawLines, "")
			}
		}
	} else {
		rawLines = strings.Split(area.View(), "\n")
	}
	for len(rawLines) < ZoomedEditorHeight-3 {
		rawLines = append(rawLines, "")
	}
	if len(rawLines) > ZoomedEditorHeight-3 {
		rawLines = rawLines[:ZoomedEditorHeight-3]
	}

	var rows []string
	rows = append(rows, topBorder)
	for _, rl := range rawLines {
		rows = append(rows, rail+PadToWidth(ClipToWidth(rl, innerW, ""), innerW))
	}

	pIcon := ProviderIcon(meta.ProviderName, meta.ModelName, cfg.Icons)
	var leftMeta []string
	if pIcon != "" {
		leftMeta = append(leftMeta, t.Fg(t.Accent, pIcon))
	}
	modelLabel := meta.ModelName
	if modelLabel == "" {
		modelLabel = "model"
	}
	leftMeta = append(leftMeta, t.BoldFg(t.Text, modelLabel))
	if meta.VimBadge != "" {
		if meta.VimBadge == "NORMAL" {
			leftMeta = append(leftMeta, t.BoldFg(t.Accent, "-- NORMAL --"))
		} else {
			leftMeta = append(leftMeta, t.Fg(t.Success, "-- INSERT --"))
		}
	}
	if meta.SelectedAll {
		leftMeta = append(leftMeta, t.BoldFg(t.Warning, "[ALL]"))
	}
	metaLine := composeLeftRight(strings.Join(leftMeta, " "), t.Fg(t.Muted, meta.RightHint), innerW)
	rows = append(rows, rail+PadToWidth(metaLine, innerW), botBorder)
	return strings.Join(rows, "\n")
}

// EditorHeight 返回当前编辑器样式占用的固定行数，供 Viewport 精确扣减高度
func EditorHeight(cfg UIConfig) int {
	switch cfg.EditorStyle {
	case "accent-rail":
		return 2
	case "minimalist":
		return 3
	default: // "opencode"
		return 6
	}
}

func composeLeftRight(left, right string, width int) string {
	if right == "" {
		return ClipToWidth(left, width, "…")
	}
	rw := VisibleWidth(right)
	if rw >= width-4 {
		return ClipToWidth(right, width, "…")
	}
	lw := width - rw - 1
	clippedLeft := ClipToWidth(left, lw, "…")
	gap := max(width-VisibleWidth(clippedLeft)-rw, 1)
	return clippedLeft + strings.Repeat(" ", gap) + right
}

// FormatTokenCompact 将 token 数格式化为 1.2k / 18.4k
func FormatTokenCompact(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1_000_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000.0)
	}
	return fmt.Sprintf("%.2fM", float64(n)/1_000_000.0)
}
