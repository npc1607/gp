package tui

import "strings"

func (m *MainUIModel) activeInputText() string {
	if m.editorZoomed {
		return m.zoomArea.Value()
	}
	return m.inputArea.Value()
}

func (m *MainUIModel) setActiveInputText(text string) {
	if m.editorZoomed {
		m.zoomArea.SetValue(text)
		return
	}
	m.inputArea.SetValue(text)
	m.inputArea.CursorEnd()
}

func (m *MainUIModel) toggleEditorZoom() {
	if m.editorZoomed {
		// 切回紧凑输入框：保留当前文本（将换行折叠为空格以便单行展示）
		text := m.zoomArea.Value()
		m.editorZoomed = false
		m.zoomArea.Blur()
		m.inputArea.Focus()
		m.inputArea.SetValue(text)
		m.inputArea.CursorEnd()
	} else {
		// 切入放大多行编辑器
		text := m.inputArea.Value()
		m.editorZoomed = true
		m.inputArea.Blur()
		m.zoomArea.Focus()
		m.zoomArea.SetValue(text)
	}
	m.resizeAllBuffers()
}

func (m *MainUIModel) submitActiveEditor() {
	val := strings.TrimSpace(m.activeInputText())
	if val == "" {
		return
	}
	if m.inputHistory != nil {
		m.inputHistory.Push(val)
	}
	if m.editorZoomed {
		m.zoomArea.Reset()
	} else {
		m.inputArea.Reset()
	}
	if m.vim != nil && m.vim.Enabled {
		m.vim.Mode = VimInsert
	}
	if m.handleLocalCommand(val) {
		return
	}
	m.submitUserPrompt(val)
}

func (m *MainUIModel) navigateHistoryPrev() {
	if m.inputHistory == nil {
		return
	}
	if prev, ok := m.inputHistory.Prev(m.activeInputText()); ok {
		m.setActiveInputText(prev)
	}
}

func (m *MainUIModel) navigateHistoryNext() {
	if m.inputHistory == nil {
		return
	}
	if next, ok := m.inputHistory.Next(); ok {
		m.setActiveInputText(next)
	}
}
