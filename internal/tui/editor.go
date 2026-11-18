package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
)

// WorkingSpinners 对应 pi-zentui 的 5 套工作行动画预设
var WorkingSpinners = map[string][]string{
	"braille":         {"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"},
	"star-bloom":      {"·", "✦", "✧", "✶", "✧", "✦"},
	"pinwheel":        {"-", "\\", "|", "/"},
	"claude-inspired": {"·", "✢", "✳", "✶", "✻", "✽"},
	"pulse":           {"⠀⠶⠀", "⠰⣿⠆", "⢾⣉⡷", "⣏⠀⣹", "⡁⠀⢈"},
}

// WorkingLineState 维护编辑器顶框内嵌的运行中状态行
type WorkingLineState struct {
	Active    bool
	TaskDesc  string
	ToolName  string
	StartedAt time.Time
	FrameIdx  int
}

func (w *WorkingLineState) Advance() {
	if w.Active {
		w.FrameIdx++
	}
}

func (w *WorkingLineState) SpinnerGlyph(preset string) string {
	frames, ok := WorkingSpinners[preset]
	if !ok || len(frames) == 0 {
		frames = WorkingSpinners["claude-inspired"]
	}
	return frames[w.FrameIdx%len(frames)]
}

func (w *WorkingLineState) RenderLabel(t Theme, cfg UIConfig) string {
	if !w.Active {
		return ""
	}
	glyph := w.SpinnerGlyph(cfg.WorkingSpinner)
	desc := w.TaskDesc
	if desc == "" {
		desc = "Simmering…"
	}
	elapsed := time.Since(w.StartedAt)
	if w.StartedAt.IsZero() {
		elapsed = 0
	}
	parts := []string{
		t.BoldFg(t.Accent, glyph),
		t.BoldFg(t.Text, desc),
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
	if meta.ThinkingLevel != "" && meta.ThinkingLevel != "off" {
		leftMeta = append(leftMeta, t.Fg(t.Warning, meta.ThinkingLevel))
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
