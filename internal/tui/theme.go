package tui

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Theme 定义 pi-tidy 与 pi-zentui 的语义化调色板 (Catppuccin Mocha / TokyoNight 风格，透明背景)
type Theme struct {
	Accent      lipgloss.Color
	Border      lipgloss.Color
	BorderMuted lipgloss.Color
	Success     lipgloss.Color
	Error       lipgloss.Color
	Warning     lipgloss.Color
	Muted       lipgloss.Color
	Dim         lipgloss.Color
	Text        lipgloss.Color
	Thinking    lipgloss.Color

	// Syntax / Bash highlight
	SyntaxKeyword  lipgloss.Color
	SyntaxFunction lipgloss.Color
	SyntaxString   lipgloss.Color
	SyntaxVariable lipgloss.Color
	SyntaxOperator lipgloss.Color
	SyntaxNumber   lipgloss.Color
	SyntaxComment  lipgloss.Color

	// Diff
	AddedFg     lipgloss.Color
	AddedBg     lipgloss.Color
	AddedEmph   lipgloss.Color
	RemovedFg   lipgloss.Color
	RemovedBg   lipgloss.Color
	RemovedEmph lipgloss.Color

	// Tool & Message borders
	ToolColors    map[string]lipgloss.Color
	MessageColors map[string]lipgloss.Color
}

// DefaultTheme 返回标准透明背景主题
func DefaultTheme() Theme {
	return Theme{
		Accent:      lipgloss.Color("#89b4fa"),
		Border:      lipgloss.Color("#585b70"),
		BorderMuted: lipgloss.Color("#45475a"),
		Success:     lipgloss.Color("#a6e3a1"),
		Error:       lipgloss.Color("#f38ba8"),
		Warning:     lipgloss.Color("#f9e2af"),
		Muted:       lipgloss.Color("#6c7086"),
		Dim:         lipgloss.Color("#585b70"),
		Text:        lipgloss.Color("#cdd6f4"),
		Thinking:    lipgloss.Color("#f9e2af"),

		SyntaxKeyword:  lipgloss.Color("#cba6f7"),
		SyntaxFunction: lipgloss.Color("#89b4fa"),
		SyntaxString:   lipgloss.Color("#a6e3a1"),
		SyntaxVariable: lipgloss.Color("#f9e2af"),
		SyntaxOperator: lipgloss.Color("#94e2d5"),
		SyntaxNumber:   lipgloss.Color("#fab387"),
		SyntaxComment:  lipgloss.Color("#6c7086"),

		AddedFg:     lipgloss.Color("#a6e3a1"),
		AddedBg:     lipgloss.Color("#1e332a"),
		AddedEmph:   lipgloss.Color("#2d4f3e"),
		RemovedFg:   lipgloss.Color("#f38ba8"),
		RemovedBg:   lipgloss.Color("#361f28"),
		RemovedEmph: lipgloss.Color("#542d3b"),

		ToolColors: map[string]lipgloss.Color{
			"bash":  lipgloss.Color("#fab387"),
			"read":  lipgloss.Color("#89b4fa"),
			"edit":  lipgloss.Color("#cba6f7"),
			"write": lipgloss.Color("#cba6f7"),
			"grep":  lipgloss.Color("#94e2d5"),
			"find":  lipgloss.Color("#94e2d5"),
			"ls":    lipgloss.Color("#94e2d5"),
			"mcp":   lipgloss.Color("#f5c2e7"),
		},
		MessageColors: map[string]lipgloss.Color{
			"assistant":  lipgloss.Color("#89b4fa"),
			"user":       lipgloss.Color("#89b4fa"),
			"compaction": lipgloss.Color("#cba6f7"),
			"branch":     lipgloss.Color("#fab387"),
			"skill":      lipgloss.Color("#74c7ec"),
			"custom":     lipgloss.Color("#f9e2af"),
			"error":      lipgloss.Color("#f38ba8"),
		},
	}
}

func (t Theme) Fg(c lipgloss.Color, s string) string {
	return lipgloss.NewStyle().Foreground(c).Render(s)
}

func (t Theme) BoldFg(c lipgloss.Color, s string) string {
	return lipgloss.NewStyle().Foreground(c).Bold(true).Render(s)
}

func (t Theme) ItalicFg(c lipgloss.Color, s string) string {
	return lipgloss.NewStyle().Foreground(c).Italic(true).Render(s)
}

func (t Theme) ToolBorderColor(toolName string, pending, isErr bool) lipgloss.Color {
	if pending {
		return t.BorderMuted
	}
	if isErr {
		return t.Error
	}
	key := toolName
	if strings.HasPrefix(key, "mcp__") {
		key = "mcp"
	}
	if c, ok := t.ToolColors[key]; ok {
		return c
	}
	return t.Accent
}

func (t Theme) MessageColor(kind string) lipgloss.Color {
	if c, ok := t.MessageColors[kind]; ok {
		return c
	}
	return t.Accent
}

// ── Width & Truncation helpers (ANSI-aware) ─────────────────────────────────

func VisibleWidth(s string) int {
	return ansi.StringWidth(s)
}

func ClipToWidth(s string, width int, tail string) string {
	if width <= 0 {
		return ""
	}
	if VisibleWidth(s) <= width {
		return s
	}
	return ansi.Truncate(s, width, tail)
}

func PadToWidth(s string, width int) string {
	if width <= 0 {
		return ""
	}
	w := VisibleWidth(s)
	if w == width {
		return s
	}
	if w < width {
		return s + strings.Repeat(" ", width-w)
	}
	return ansi.Truncate(s, width, "")
}

// ── Icons (Nerd Font + ASCII fallback) ──────────────────────────────────────

var providerIcons = map[string]string{
	"openai":            "\uec81",
	"openai-codex":      "\uec81",
	"anthropic":         "\uec82",
	"deepseek":          "🐳",
	"google":            "\U000f02ad",
	"google-gemini-cli": "\U000f02ad",
	"google-vertex":     "\U000f02ad",
	"antigravity":       "\U000f02ad",
}

func ProviderIcon(provider, model string, icons bool) string {
	if !icons {
		return ""
	}
	p := strings.ToLower(provider)
	m := strings.ToLower(model)
	if ic, ok := providerIcons[p]; ok {
		return ic
	}
	switch {
	case strings.Contains(m, "claude"):
		return "\uec82"
	case strings.Contains(m, "gpt") || strings.Contains(m, "o1") || strings.Contains(m, "o3") || strings.Contains(m, "codex"):
		return "\uec81"
	case strings.Contains(m, "gemini"):
		return "\U000f02ad"
	case strings.Contains(m, "deepseek"):
		return "🐳"
	default:
		return "\U000f06a9"
	}
}

func ToolKindIcon(name string, icons bool) string {
	if !icons {
		return ""
	}
	if strings.HasPrefix(name, "mcp__") {
		return "\U000f051f"
	}
	switch name {
	case "bash":
		return "\uf489"
	case "read":
		return "\U000f0219"
	case "edit":
		return "\uf044"
	case "write":
		return "\uf040"
	case "grep":
		return "\uf002"
	case "find":
		return "\U000f021e"
	case "ls":
		return "\uf07c"
	case "subagent", "ask_peer":
		return "\U000f06a9"
	default:
		return "\uf0ad"
	}
}

type fileIconDef struct {
	icon  string
	color lipgloss.Color
}

var extIcons = map[string]fileIconDef{
	".go":   {icon: "\ue627", color: lipgloss.Color("#00add8")},
	".ts":   {icon: "\ue628", color: lipgloss.Color("#3178c6")},
	".tsx":  {icon: "\ue7ba", color: lipgloss.Color("#3178c6")},
	".js":   {icon: "\ue74e", color: lipgloss.Color("#f7df1e")},
	".jsx":  {icon: "\ue7ba", color: lipgloss.Color("#61dafb")},
	".py":   {icon: "\ue73c", color: lipgloss.Color("#3776ab")},
	".rs":   {icon: "\ue7a8", color: lipgloss.Color("#dea584")},
	".json": {icon: "\ue60b", color: lipgloss.Color("#cbcb41")},
	".yaml": {icon: "\uf481", color: lipgloss.Color("#cb171e")},
	".yml":  {icon: "\uf481", color: lipgloss.Color("#cb171e")},
	".md":   {icon: "\uf48a", color: lipgloss.Color("#519aba")},
	".sh":   {icon: "\uf489", color: lipgloss.Color("#4d5a5e")},
	".zsh":  {icon: "\uf489", color: lipgloss.Color("#4d5a5e")},
	".sql":  {icon: "\uf1c0", color: lipgloss.Color("#dad8d8")},
	".html": {icon: "\ue736", color: lipgloss.Color("#e44d26")},
	".css":  {icon: "\ue749", color: lipgloss.Color("#42a5f5")},
	".toml": {icon: "\ue6b2", color: lipgloss.Color("#9c4221")},
}

func FileIcon(path string, icons bool) string {
	if !icons || path == "" {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(path))
	if def, ok := extIcons[ext]; ok {
		return lipgloss.NewStyle().Foreground(def.color).Render(def.icon)
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("#89b4fa")).Render("\uf15b")
}

func UserIcon(icons bool) string {
	if !icons {
		return "User"
	}
	return "\uf007"
}

func ThinkingIcon(icons bool) string {
	if !icons {
		return ""
	}
	return "🧠"
}

func CopyChip(icons bool) string {
	if icons {
		return "\U000f0192"
	}
	return "copy"
}

// ── Frame & Border Drawing (pi-tidy rounded transparent frames) ─────────────

func FormatMessageTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	now := time.Now()
	if ts.Year() == now.Year() && ts.YearDay() == now.YearDay() {
		return ts.Format("15:04")
	}
	return ts.Format("01-02 15:04")
}

func MessageHeaderLabel(t Theme, color lipgloss.Color, identity, timeStr, style string, width int) (string, int) {
	if timeStr == "" {
		return identity, 1
	}
	inline := identity + " " + timeStr
	if style != "split" {
		return inline, 1
	}
	split := identity + " " + t.Fg(color, "──") + " " + timeStr
	if VisibleWidth(split)+8 <= width {
		return split, 2
	}
	return inline, 1
}

// LabeledTopBorder 绘制带标签的圆角顶边框: `╭─ label ─────╮`
func LabeledTopBorder(width int, t Theme, color lipgloss.Color, label string, align string, lead int) string {
	if width < 4 {
		width = 4
	}
	inner := width - 2
	if label == "" {
		return t.Fg(color, "╭"+strings.Repeat("─", inner)+"╮")
	}
	lw := VisibleWidth(label)
	rest := inner - 2 - lead - lw
	if rest < 1 {
		return t.Fg(color, "╭"+strings.Repeat("─", inner)+"╮")
	}
	start := strings.Repeat("─", lead)
	if align == "right" {
		return t.Fg(color, "╭"+strings.Repeat("─", rest)+" ") + label + t.Fg(color, " "+start+"╮")
	}
	return t.Fg(color, "╭"+start+" ") + label + t.Fg(color, " "+strings.Repeat("─", rest)+"╮")
}

// LabeledRule 绘制带标签的水平分割线: `─ label ─────`
func LabeledRule(width int, t Theme, color lipgloss.Color, label string, align string, lead int) string {
	if width < 2 {
		width = 2
	}
	if label == "" {
		return t.Fg(color, strings.Repeat("─", width))
	}
	rest := width - 2 - lead - VisibleWidth(label)
	if rest < 1 {
		return t.Fg(color, strings.Repeat("─", width))
	}
	start := strings.Repeat("─", lead)
	if align == "right" {
		return t.Fg(color, strings.Repeat("─", rest)+" ") + label + t.Fg(color, " "+start)
	}
	return t.Fg(color, start+" ") + label + t.Fg(color, " "+strings.Repeat("─", rest))
}

// DrawFrame 绘制 pi-tidy 风格透明圆角边框 (╭╮╰╯)
func DrawFrame(lines []string, width int, t Theme, color lipgloss.Color, topLabel string, align string, lead int) []string {
	if width < 4 {
		width = 4
	}
	inner := width - 2
	out := make([]string, 0, len(lines)+2)
	out = append(out, LabeledTopBorder(width, t, color, topLabel, align, lead))
	bar := t.Fg(color, "│")
	for _, line := range lines {
		out = append(out, bar+PadToWidth(line, inner)+bar)
	}
	out = append(out, t.Fg(color, "╰"+strings.Repeat("─", inner)+"╯"))
	return out
}
