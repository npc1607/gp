package tui

import (
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

// VimMode 表示 Vim 编辑器的当前模式
type VimMode string

const (
	VimInsert VimMode = "INSERT"
	VimNormal VimMode = "NORMAL"
)

// VimState 管理可开关的 Vim 编辑模式状态
type VimState struct {
	Enabled   bool
	Mode      VimMode
	pendingOp string // 如 "d" 等待第二个 "d" 执行 dd
}

// NewVimState 创建默认关闭的 Vim 状态
func NewVimState() *VimState {
	return &VimState{
		Enabled: false,
		Mode:    VimInsert,
	}
}

// Toggle 切换 Vim 模式开关（开启时默认进入 INSERT 模式）
func (v *VimState) Toggle() bool {
	v.Enabled = !v.Enabled
	v.Mode = VimInsert
	v.pendingOp = ""
	return v.Enabled
}

// SetEnabled 显式开启或关闭 Vim 模式
func (v *VimState) SetEnabled(on bool) {
	v.Enabled = on
	v.Mode = VimInsert
	v.pendingOp = ""
}

// HandleCompactKey 在单行紧凑输入框中处理 Vim 按键，返回是否已消费事件
func (m *MainUIModel) HandleVimKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	if m.vim == nil || !m.vim.Enabled {
		return nil, false
	}
	key := msg.String()

	// INSERT 模式：Esc 回到 NORMAL（除非主 Agent 正在运行需要中断，交由 Keymap 处理）
	if m.vim.Mode == VimInsert {
		if key == "esc" && !m.primaryBusy {
			m.vim.Mode = VimNormal
			m.vim.pendingOp = ""
			return nil, true
		}
		return nil, false
	}

	// NORMAL 模式
	if m.vim.pendingOp == "d" {
		m.vim.pendingOp = ""
		if key == "d" {
			m.setActiveInputText("")
			return nil, true
		}
	}

	switch key {
	case "i":
		m.vim.Mode = VimInsert
		return nil, true
	case "a":
		m.vim.Mode = VimInsert
		if !m.editorZoomed {
			m.inputArea.SetCursor(m.inputArea.Position() + 1)
		}
		return nil, true
	case "I", "0", "^":
		if key == "I" {
			m.vim.Mode = VimInsert
		}
		if !m.editorZoomed {
			m.inputArea.CursorStart()
		}
		return nil, true
	case "A", "$":
		if key == "A" {
			m.vim.Mode = VimInsert
		}
		if !m.editorZoomed {
			m.inputArea.CursorEnd()
		}
		return nil, true
	case "h":
		if m.editorZoomed {
			var cmd tea.Cmd
			m.zoomArea, cmd = m.zoomArea.Update(tea.KeyMsg{Type: tea.KeyLeft})
			return cmd, true
		}
		m.inputArea.SetCursor(max(m.inputArea.Position()-1, 0))
		return nil, true
	case "l":
		if m.editorZoomed {
			var cmd tea.Cmd
			m.zoomArea, cmd = m.zoomArea.Update(tea.KeyMsg{Type: tea.KeyRight})
			return cmd, true
		}
		m.inputArea.SetCursor(m.inputArea.Position() + 1)
		return nil, true
	case "j":
		if m.editorZoomed {
			var cmd tea.Cmd
			m.zoomArea, cmd = m.zoomArea.Update(tea.KeyMsg{Type: tea.KeyDown})
			return cmd, true
		}
		m.navigateHistoryNext()
		return nil, true
	case "k":
		if m.editorZoomed {
			var cmd tea.Cmd
			m.zoomArea, cmd = m.zoomArea.Update(tea.KeyMsg{Type: tea.KeyUp})
			return cmd, true
		}
		m.navigateHistoryPrev()
		return nil, true
	case "w":
		if !m.editorZoomed {
			m.moveWordForward()
		}
		return nil, true
	case "b":
		if !m.editorZoomed {
			m.moveWordBackward()
		}
		return nil, true
	case "x":
		if !m.editorZoomed {
			rs := []rune(m.inputArea.Value())
			pos := m.inputArea.Position()
			if pos >= 0 && pos < len(rs) {
				next := append(rs[:pos], rs[pos+1:]...)
				m.inputArea.SetValue(string(next))
				m.inputArea.SetCursor(min(pos, len(next)))
			}
		}
		return nil, true
	case "d":
		m.vim.pendingOp = "d"
		return nil, true
	case "enter":
		// NORMAL 模式下按 Enter 直接提交当前内容
		m.submitActiveEditor()
		return nil, true
	case "esc":
		return nil, false
	default:
		// NORMAL 模式下拦截普通字母，防止误输入
		if msg.Type == tea.KeyRunes {
			return nil, true
		}
		return nil, false
	}
}

func (m *MainUIModel) moveWordForward() {
	rs := []rune(m.inputArea.Value())
	pos := m.inputArea.Position()
	n := len(rs)
	for pos < n && !unicode.IsSpace(rs[pos]) {
		pos++
	}
	for pos < n && unicode.IsSpace(rs[pos]) {
		pos++
	}
	m.inputArea.SetCursor(pos)
}

func (m *MainUIModel) moveWordBackward() {
	rs := []rune(m.inputArea.Value())
	pos := min(m.inputArea.Position(), len(rs))
	if pos > 0 {
		pos--
	}
	for pos > 0 && unicode.IsSpace(rs[pos]) {
		pos--
	}
	for pos > 0 && !unicode.IsSpace(rs[pos-1]) {
		pos--
	}
	m.inputArea.SetCursor(pos)
}
