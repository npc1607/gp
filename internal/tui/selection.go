package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// TextSelectionState 维护 TUI 全屏鼠标左键拖选文字的实时选区与复制提示状态
type TextSelectionState struct {
	Active     bool
	HasRange   bool
	AnchorX    int
	AnchorY    int
	EndX       int
	EndY       int
	Toast      string
	ToastUntil time.Time
}

func (s *TextSelectionState) Start(x, y int) {
	s.Active = true
	s.HasRange = false
	s.AnchorX = x
	s.AnchorY = y
	s.EndX = x
	s.EndY = y
}

func (s *TextSelectionState) Update(x, y int) {
	if !s.Active {
		return
	}
	s.EndX = x
	s.EndY = y
	if x != s.AnchorX || y != s.AnchorY {
		s.HasRange = true
	}
}

func (s *TextSelectionState) Clear() {
	s.Active = false
	s.HasRange = false
}

func (s *TextSelectionState) SetToast(msg string, d time.Duration) {
	s.Toast = msg
	s.ToastUntil = time.Now().Add(d)
}

func (s *TextSelectionState) ActiveToast() string {
	if s.Toast == "" || time.Now().After(s.ToastUntil) {
		return ""
	}
	return s.Toast
}

// normalizedBounds 返回按行列排序后的起止坐标 (startY, startX) <= (endY, endX)
func (s TextSelectionState) normalizedBounds() (startY, startX, endY, endX int) {
	if s.AnchorY < s.EndY || (s.AnchorY == s.EndY && s.AnchorX <= s.EndX) {
		return s.AnchorY, s.AnchorX, s.EndY, s.EndX
	}
	return s.EndY, s.EndX, s.AnchorY, s.AnchorX
}

// ExtractSelectedText 从渲染后的 TUI 帧文本中按终端单元格坐标提取选中的纯文本（CJK 宽字符安全）
func ExtractSelectedText(renderedView string, sel TextSelectionState) string {
	if !sel.HasRange {
		return ""
	}
	lines := strings.Split(renderedView, "\n")
	if len(lines) == 0 {
		return ""
	}
	startY, startX, endY, endX := sel.normalizedBounds()
	if startY < 0 {
		startY = 0
	}
	if endY >= len(lines) {
		endY = len(lines) - 1
	}
	if startY > endY {
		return ""
	}

	var out []string
	for y := startY; y <= endY; y++ {
		plain := ansi.Strip(lines[y])
		lineW := ansi.StringWidth(plain)
		c0, c1 := 0, lineW
		if y == startY {
			c0 = max(startX, 0)
		}
		if y == endY {
			c1 = min(endX+1, lineW)
		}
		if c0 >= c1 {
			out = append(out, "")
			continue
		}
		seg := sliceByDisplayCols(plain, c0, c1)
		out = append(out, strings.TrimRight(seg, " "))
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

// sliceByDisplayCols 按终端显示列区间 [c0, c1) 截取纯文本，正确处理中文等双宽字符
func sliceByDisplayCols(plain string, c0, c1 int) string {
	var sb strings.Builder
	col := 0
	for _, r := range plain {
		rw := ansi.StringWidth(string(r))
		if rw <= 0 {
			rw = 1
		}
		if col+rw > c0 && col < c1 {
			sb.WriteRune(r)
		}
		col += rw
		if col >= c1 {
			break
		}
	}
	return sb.String()
}

// ApplySelectionHighlight 在当前渲染帧上实时叠加选区高亮背景
func ApplySelectionHighlight(renderedView string, sel TextSelectionState, t Theme) string {
	if !sel.Active || !sel.HasRange {
		return renderedView
	}
	lines := strings.Split(renderedView, "\n")
	startY, startX, endY, endX := sel.normalizedBounds()
	if startY < 0 {
		startY = 0
	}
	if endY >= len(lines) {
		endY = len(lines) - 1
	}
	if startY > endY {
		return renderedView
	}

	hlStyle := lipgloss.NewStyle().
		Background(lipgloss.Color("#45475a")).
		Foreground(t.Text)

	for y := startY; y <= endY; y++ {
		line := lines[y]
		lineW := VisibleWidth(line)
		c0, c1 := 0, lineW
		if y == startY {
			c0 = max(startX, 0)
		}
		if y == endY {
			c1 = min(endX+1, lineW)
		}
		if c0 >= c1 || c0 >= lineW {
			continue
		}
		prefix := ansi.Cut(line, 0, c0)
		midPlain := ansi.Strip(ansi.Cut(line, c0, c1))
		suffix := ansi.Cut(line, c1, lineW)
		lines[y] = prefix + hlStyle.Render(midPlain) + suffix
	}
	return strings.Join(lines, "\n")
}
