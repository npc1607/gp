package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// AskUserOption 对应 rpiv-ask-user-question 的单个选项
type AskUserOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
	Preview     string `json:"preview,omitempty"`
}

// AskUserQuestion 对应 rpiv-ask-user-question 的单个问题定义
type AskUserQuestion struct {
	Question    string          `json:"question"`
	Header      string          `json:"header"`
	Options     []AskUserOption `json:"options"`
	MultiSelect bool            `json:"multiSelect,omitempty"`
}

// AskUserAnswer 对应 rpiv-ask-user-question 的结构化单题答案
type AskUserAnswer struct {
	QuestionIndex int      `json:"questionIndex"`
	Question      string   `json:"question"`
	Kind          string   `json:"kind"` // "option" | "custom" | "multi"
	Answer        string   `json:"answer"`
	Selected      []string `json:"selected,omitempty"`
	Notes         string   `json:"notes,omitempty"`
	Preview       string   `json:"preview,omitempty"`
}

// AskUserResult 对应问卷最终提交或取消的结果信封
type AskUserResult struct {
	Answers    []AskUserAnswer `json:"answers"`
	GlobalNote string          `json:"globalNote,omitempty"`
	Cancelled  bool            `json:"cancelled"`
}

// AskUserQuestionMsg 由 ask_user_question 工具发送给 Bubbletea 的交互式问卷消息
type AskUserQuestionMsg struct {
	Questions  []AskUserQuestion
	ResponseCh chan<- AskUserResult
}

type questionDraft struct {
	Cursor       int
	SinglePicked int // -1 = none, >=0 = option index, -2 = custom
	MultiChecked map[int]bool
	CustomText   string
	Note         string
}

// AskUserDialogState 管理终端底部 rpiv-ask-user-question 多标签问卷交互状态
type AskUserDialogState struct {
	Active       bool
	Collapsed    bool
	Questions    []AskUserQuestion
	ResponseCh   chan<- AskUserResult
	ActiveTab    int // 0..len(Questions)-1; len(Questions) = Submit tab (when >1 question)
	Drafts       []questionDraft
	SubmitCursor int // 0 = Submit answers, 1 = Cancel
	GlobalNote   string

	EditingCustom bool
	EditingNote   bool
	Input         textinput.Model
}

func NewAskUserDialogState(msg AskUserQuestionMsg) *AskUserDialogState {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.CharLimit = 1024
	ti.Width = 64
	ti.Focus()

	drafts := make([]questionDraft, len(msg.Questions))
	for i := range drafts {
		drafts[i] = questionDraft{
			SinglePicked: -1,
			MultiChecked: make(map[int]bool),
		}
	}
	editingCustom := len(msg.Questions) > 0 && len(msg.Questions[0].Options) == 0
	return &AskUserDialogState{
		Active:        true,
		Questions:     msg.Questions,
		ResponseCh:    msg.ResponseCh,
		Drafts:        drafts,
		EditingCustom: editingCustom,
		Input:         ti,
	}
}

func (d *AskUserDialogState) totalTabs() int {
	if len(d.Questions) <= 1 {
		return len(d.Questions)
	}
	return len(d.Questions) + 1 // extra Submit tab
}

func (d *AskUserDialogState) isSubmitTab() bool {
	return len(d.Questions) > 1 && d.ActiveTab == len(d.Questions)
}

func (d *AskUserDialogState) isQuestionAnswered(idx int) bool {
	if idx < 0 || idx >= len(d.Questions) {
		return false
	}
	dr := d.Drafts[idx]
	q := d.Questions[idx]
	if q.MultiSelect {
		for _, ok := range dr.MultiChecked {
			if ok {
				return true
			}
		}
		return strings.TrimSpace(dr.CustomText) != ""
	}
	if dr.SinglePicked >= 0 && dr.SinglePicked < len(q.Options) {
		return true
	}
	return dr.SinglePicked == -2 && strings.TrimSpace(dr.CustomText) != ""
}

func (d *AskUserDialogState) buildResult(cancelled bool) AskUserResult {
	if cancelled {
		return AskUserResult{Cancelled: true, GlobalNote: d.GlobalNote}
	}
	var answers []AskUserAnswer
	for i, q := range d.Questions {
		dr := d.Drafts[i]
		if q.MultiSelect {
			var selected []string
			for optIdx, opt := range q.Options {
				if dr.MultiChecked[optIdx] {
					selected = append(selected, opt.Label)
				}
			}
			custom := strings.TrimSpace(dr.CustomText)
			if len(selected) == 0 && custom == "" && dr.Note == "" {
				continue
			}
			if len(selected) > 0 || custom != "" {
				answers = append(answers, AskUserAnswer{
					QuestionIndex: i,
					Question:      q.Question,
					Kind:          "multi",
					Answer:        custom,
					Selected:      selected,
					Notes:         dr.Note,
				})
			}
			continue
		}
		if dr.SinglePicked >= 0 && dr.SinglePicked < len(q.Options) {
			opt := q.Options[dr.SinglePicked]
			answers = append(answers, AskUserAnswer{
				QuestionIndex: i,
				Question:      q.Question,
				Kind:          "option",
				Answer:        opt.Label,
				Notes:         dr.Note,
				Preview:       opt.Preview,
			})
		} else if dr.SinglePicked == -2 && strings.TrimSpace(dr.CustomText) != "" {
			answers = append(answers, AskUserAnswer{
				QuestionIndex: i,
				Question:      q.Question,
				Kind:          "custom",
				Answer:        strings.TrimSpace(dr.CustomText),
				Notes:         dr.Note,
			})
		}
	}
	return AskUserResult{
		Answers:    answers,
		GlobalNote: d.GlobalNote,
		Cancelled:  false,
	}
}

func (d *AskUserDialogState) finish(cancelled bool) {
	if !d.Active {
		return
	}
	res := d.buildResult(cancelled)
	d.Active = false
	if d.ResponseCh != nil {
		select {
		case d.ResponseCh <- res:
		default:
		}
	}
}

// HandleAskUserKey 处理问卷弹窗打开时的键盘事件
func (m *MainUIModel) HandleAskUserKey(msg tea.KeyMsg) bool {
	d := m.askUser
	if d == nil || !d.Active {
		return false
	}
	key := msg.String()

	// Ctrl+] 折叠/展开问卷，方便回看上方对话
	if key == "ctrl+]" {
		d.Collapsed = !d.Collapsed
		m.resizeAllBuffers()
		return true
	}
	// PageUp / PageDown 保持滚动上方对话区
	if key == "pgup" {
		if buf, ok := m.agentBuffers[m.activeTabID]; ok {
			buf.Scroll(-5)
		}
		return true
	}
	if key == "pgdown" {
		if buf, ok := m.agentBuffers[m.activeTabID]; ok {
			buf.Scroll(5)
		}
		return true
	}
	if key == "ctrl+c" {
		d.finish(true)
		m.resizeAllBuffers()
		return false
	}
	if d.Collapsed {
		return false
	}

	// 处于自定义回答或备注输入态
	if d.EditingCustom || d.EditingNote {
		switch key {
		case "esc":
			d.EditingCustom = false
			d.EditingNote = false
			d.Input.Reset()
			return true
		case "ctrl+u":
			d.Input.Reset()
			return true
		case "enter":
			val := strings.TrimSpace(d.Input.Value())
			d.Input.Reset()
			if d.EditingNote {
				d.EditingNote = false
				if d.isSubmitTab() {
					d.GlobalNote = val
				} else if d.ActiveTab < len(d.Drafts) {
					d.Drafts[d.ActiveTab].Note = val
				}
				return true
			}
			// EditingCustom
			d.EditingCustom = false
			dr := &d.Drafts[d.ActiveTab]
			q := d.Questions[d.ActiveTab]
			dr.CustomText = val
			if !q.MultiSelect {
				if val != "" {
					dr.SinglePicked = -2
					if len(d.Questions) == 1 {
						d.finish(false)
						m.resizeAllBuffers()
						return true
					}
					d.ActiveTab++
				}
			}
			return true
		default:
			d.Input, _ = d.Input.Update(msg)
			return true
		}
	}

	switch key {
	case "esc":
		d.finish(true)
		m.resizeAllBuffers()
		return true
	case "tab", "right":
		if d.totalTabs() > 1 {
			d.ActiveTab = (d.ActiveTab + 1) % d.totalTabs()
		}
		return true
	case "shift+tab", "left":
		if d.totalTabs() > 1 {
			d.ActiveTab = (d.ActiveTab - 1 + d.totalTabs()) % d.totalTabs()
		}
		return true
	case "n":
		d.EditingNote = true
		d.Input.Focus()
		if d.isSubmitTab() {
			d.Input.SetValue(d.GlobalNote)
		} else {
			d.Input.SetValue(d.Drafts[d.ActiveTab].Note)
		}
		d.Input.CursorEnd()
		return true
	}

	// Submit Tab 导航
	if d.isSubmitTab() {
		switch key {
		case "up":
			if d.SubmitCursor > 0 {
				d.SubmitCursor--
			}
		case "down":
			if d.SubmitCursor < 1 {
				d.SubmitCursor++
			}
		case "enter":
			d.finish(d.SubmitCursor == 1)
			m.resizeAllBuffers()
		}
		return true
	}

	// Question Tab 导航
	q := d.Questions[d.ActiveTab]
	dr := &d.Drafts[d.ActiveTab]
	typeSomethingIdx := len(q.Options)
	maxRow := typeSomethingIdx
	if q.MultiSelect {
		maxRow = typeSomethingIdx + 1 // Next row
	}

	switch key {
	case "up":
		if dr.Cursor > 0 {
			dr.Cursor--
		}
	case "down":
		if dr.Cursor < maxRow {
			dr.Cursor++
		}
	case "1", "2", "3", "4":
		if !q.MultiSelect {
			idx := int(key[0] - '1')
			if idx >= 0 && idx < len(q.Options) {
				dr.Cursor = idx
				dr.SinglePicked = idx
				if len(d.Questions) == 1 {
					d.finish(false)
					m.resizeAllBuffers()
					return true
				}
				d.ActiveTab++
			} else if idx == typeSomethingIdx {
				dr.Cursor = typeSomethingIdx
				d.EditingCustom = true
				d.Input.Focus()
				d.Input.SetValue(dr.CustomText)
				d.Input.CursorEnd()
				return true
			}
		}
	case " ":
		if q.MultiSelect && dr.Cursor < len(q.Options) {
			dr.MultiChecked[dr.Cursor] = !dr.MultiChecked[dr.Cursor]
		}
	case "enter":
		if dr.Cursor == typeSomethingIdx {
			d.EditingCustom = true
			d.Input.Focus()
			d.Input.SetValue(dr.CustomText)
			d.Input.CursorEnd()
			return true
		}
		if q.MultiSelect {
			if dr.Cursor == maxRow { // Next
				d.ActiveTab++
				return true
			}
			dr.MultiChecked[dr.Cursor] = !dr.MultiChecked[dr.Cursor]
			return true
		}
		dr.SinglePicked = dr.Cursor
		if len(d.Questions) == 1 {
			d.finish(false)
			m.resizeAllBuffers()
			return true
		}
		d.ActiveTab++
	default:
		// 光标停留在 "Type something." 或纯开放式问题（无预设选项）时，直接键入字符自动进入输入态
		if dr.Cursor == typeSomethingIdx && (msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace) {
			d.EditingCustom = true
			d.Input.Focus()
			d.Input.SetValue(dr.CustomText)
			d.Input.CursorEnd()
			d.Input, _ = d.Input.Update(msg)
			return true
		}
	}
	return true
}

// RenderAskUserDialog 渲染 rpiv-ask-user-question 风格的多标签问卷对话框
func RenderAskUserDialog(d *AskUserDialogState, width int, t Theme) string {
	if d == nil || !d.Active {
		return ""
	}
	if width < 32 {
		width = 32
	}
	if d.Collapsed {
		answered := 0
		for i := range d.Questions {
			if d.isQuestionAnswered(i) {
				answered++
			}
		}
		line := fmt.Sprintf(" ▸ %s (%d/%d answered) · %s",
			t.BoldFg(t.Accent, "Questionnaire paused"),
			answered, len(d.Questions),
			t.Fg(t.Muted, "Ctrl+] to resume"),
		)
		return ClipToWidth(line, width, "…")
	}

	innerW := width - 4
	var rows []string

	// Tab 栏
	if len(d.Questions) > 1 {
		var tabs []string
		for i, q := range d.Questions {
			lbl := q.Header
			if lbl == "" {
				lbl = fmt.Sprintf("Q%d", i+1)
			}
			mark := "○"
			if d.isQuestionAnswered(i) {
				mark = t.Fg(t.Success, "✓")
			}
			if i == d.ActiveTab {
				tabs = append(tabs, t.BoldFg(t.Accent, fmt.Sprintf("[%s %s]", mark, lbl)))
			} else {
				tabs = append(tabs, t.Fg(t.Muted, fmt.Sprintf("%s %s", mark, lbl)))
			}
		}
		subLbl := "Submit"
		if d.isSubmitTab() {
			tabs = append(tabs, t.BoldFg(t.Success, "[✓ "+subLbl+"]"))
		} else {
			tabs = append(tabs, t.Fg(t.Muted, subLbl))
		}
		rows = append(rows, ClipToWidth(strings.Join(tabs, "  "), innerW, "…"))
		rows = append(rows, t.Fg(t.BorderMuted, strings.Repeat("─", innerW)))
	}

	if d.isSubmitTab() {
		rows = append(rows, t.BoldFg(t.Text, "Review your answers:"))
		var blank []string
		for i, q := range d.Questions {
			hdr := q.Header
			if hdr == "" {
				hdr = fmt.Sprintf("Q%d", i+1)
			}
			ans := d.summarizeDraft(i)
			if ans == "" {
				blank = append(blank, hdr)
				rows = append(rows, fmt.Sprintf("  %s: %s", t.Fg(t.Muted, hdr), t.Fg(t.Warning, "(unanswered)")))
			} else {
				rows = append(rows, fmt.Sprintf("  %s: %s", t.Fg(t.Accent, hdr), t.Fg(t.Text, ClipToWidth(ans, innerW-len(hdr)-6, "…"))))
			}
		}
		if len(blank) > 0 {
			rows = append(rows, t.Fg(t.Warning, "  ⚠ Unanswered: "+strings.Join(blank, ", ")))
		}
		if d.GlobalNote != "" {
			rows = append(rows, t.Fg(t.Muted, "  Note: "+ClipToWidth(d.GlobalNote, innerW-10, "…")))
		}
		rows = append(rows, "")
		subOpts := []string{"Submit answers", "Cancel"}
		for i, label := range subOpts {
			sel := "  "
			if i == d.SubmitCursor {
				sel = t.BoldFg(t.Accent, "› ")
				rows = append(rows, sel+t.BoldFg(t.Accent, label))
			} else {
				rows = append(rows, sel+t.Fg(t.Muted, label))
			}
		}
	} else {
		q := d.Questions[d.ActiveTab]
		dr := d.Drafts[d.ActiveTab]

		chip := ""
		if q.Header != "" {
			chip = t.BoldFg(t.Accent, "["+q.Header+"] ")
		}
		rows = append(rows, wrapStyledLine(chip+t.BoldFg(t.Text, q.Question), innerW)...)
		rows = append(rows, "")

		var optRows []string
		for i, opt := range q.Options {
			sel := "  "
			if i == dr.Cursor {
				sel = t.BoldFg(t.Accent, "› ")
			}
			var mark string
			if q.MultiSelect {
				if dr.MultiChecked[i] {
					mark = t.Fg(t.Success, "[x] ")
				} else {
					mark = t.Fg(t.Muted, "[ ] ")
				}
			} else {
				mark = t.Fg(t.Muted, fmt.Sprintf("%d. ", i+1))
			}
			lbl := t.Fg(t.Text, opt.Label)
			if i == dr.Cursor {
				lbl = t.BoldFg(t.Accent, opt.Label)
			}
			optRows = append(optRows, ClipToWidth(sel+mark+lbl, innerW, "…"))
			if opt.Description != "" {
				optRows = append(optRows, t.Fg(t.Muted, "     "+ClipToWidth(opt.Description, innerW-5, "…")))
			}
		}

		// Type something.
		tsIdx := len(q.Options)
		tsSel := "  "
		if dr.Cursor == tsIdx {
			tsSel = t.BoldFg(t.Accent, "› ")
		}
		tsMark := ""
		if q.MultiSelect {
			if strings.TrimSpace(dr.CustomText) != "" {
				tsMark = t.Fg(t.Success, "[x] ")
			} else {
				tsMark = t.Fg(t.Muted, "[ ] ")
			}
		} else {
			tsMark = t.Fg(t.Muted, fmt.Sprintf("%d. ", tsIdx+1))
		}
		tsLbl := "Type something."
		if dr.CustomText != "" {
			tsLbl = "Type something.: " + ClipToWidth(dr.CustomText, innerW-24, "…")
		}
		optRows = append(optRows, ClipToWidth(tsSel+tsMark+t.ItalicFg(t.Muted, tsLbl), innerW, "…"))

		if q.MultiSelect {
			nextSel := "  "
			if dr.Cursor == tsIdx+1 {
				nextSel = t.BoldFg(t.Accent, "› ")
			}
			optRows = append(optRows, nextSel+t.BoldFg(t.Success, "→ Next"))
		}

		// Preview rendering (side-by-side if >=100 cols, stacked below otherwise)
		var focusedPreview string
		if dr.Cursor >= 0 && dr.Cursor < len(q.Options) {
			focusedPreview = q.Options[dr.Cursor].Preview
		}
		if focusedPreview != "" && width >= 100 {
			leftW := innerW * 55 / 100
			rightW := innerW - leftW - 3
			prevLines := strings.Split(strings.ReplaceAll(focusedPreview, "\r\n", "\n"), "\n")
			if len(prevLines) > 8 {
				prevLines = prevLines[:8]
			}
			prevBox := DrawFrame(prevLines, rightW, t, t.BorderMuted, t.Fg(t.Muted, "preview"), "left", 1)
			leftBlock := lipgloss.NewStyle().Width(leftW).Render(strings.Join(optRows, "\n"))
			rightBlock := strings.Join(prevBox, "\n")
			joined := lipgloss.JoinHorizontal(lipgloss.Top, leftBlock, " ", rightBlock)
			rows = append(rows, strings.Split(joined, "\n")...)
		} else {
			rows = append(rows, optRows...)
			if focusedPreview != "" {
				prevLines := strings.Split(strings.ReplaceAll(focusedPreview, "\r\n", "\n"), "\n")
				if len(prevLines) > 5 {
					prevLines = prevLines[:5]
				}
				rows = append(rows, DrawFrame(prevLines, innerW, t, t.BorderMuted, t.Fg(t.Muted, "preview"), "left", 1)...)
			}
		}

		if dr.Note != "" && !d.EditingNote {
			rows = append(rows, t.Fg(t.Muted, "  Note: "+ClipToWidth(dr.Note, innerW-10, "…")))
		}
	}

	if d.EditingCustom || d.EditingNote {
		lbl := "Custom answer: "
		if d.EditingNote {
			lbl = "Note: "
		}
		rows = append(rows, "")
		rows = append(rows, t.BoldFg(t.Accent, lbl)+d.Input.View())
	}

	rows = append(rows, t.Fg(t.BorderMuted, strings.Repeat("─", innerW)))
	rows = append(rows, t.Fg(t.Dim, "↑/↓ move · Enter select · Tab switch · n note · Ctrl+] collapse · Esc cancel"))

	padded := make([]string, len(rows))
	for i, r := range rows {
		padded[i] = " " + r
	}
	title := t.BoldFg(t.Accent, "ask_user_question")
	return strings.Join(DrawFrame(padded, width, t, t.Accent, title, "left", 1), "\n")
}

func (d *AskUserDialogState) summarizeDraft(idx int) string {
	dr := d.Drafts[idx]
	q := d.Questions[idx]
	if q.MultiSelect {
		var parts []string
		for i, opt := range q.Options {
			if dr.MultiChecked[i] {
				parts = append(parts, opt.Label)
			}
		}
		if strings.TrimSpace(dr.CustomText) != "" {
			parts = append(parts, strings.TrimSpace(dr.CustomText))
		}
		return strings.Join(parts, ", ")
	}
	if dr.SinglePicked >= 0 && dr.SinglePicked < len(q.Options) {
		return q.Options[dr.SinglePicked].Label
	}
	if dr.SinglePicked == -2 {
		return strings.TrimSpace(dr.CustomText)
	}
	return ""
}
