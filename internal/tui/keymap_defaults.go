package tui

import (
	"fmt"
	"log/slog"
	"strings"

	"gp/internal/core/auth"

	tea "github.com/charmbracelet/bubbletea"
)

// NewDefaultKeymap 创建并注册默认终端交互快捷键
func NewDefaultKeymap() *KeymapRegistry {
	km := NewKeymapRegistry()
	RegisterDefaultKeybindings(km)
	return km
}

// RegisterDefaultKeybindings 注册 TUI 主界面的标准快捷键集合
func RegisterDefaultKeybindings(km *KeymapRegistry) {
	km.Register(KeyBinding{
		ID:          "quit",
		Keys:        []string{"ctrl+c"},
		Label:       "Ctrl+C",
		Description: "Quit application",
		Category:    "system",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			return tea.Quit, true
		},
	})

	km.Register(KeyBinding{
		ID:          "interrupt_or_rollback",
		Keys:        []string{"esc"},
		Label:       "Esc",
		Description: "Interrupt active agent turn, or rollback/clear input when idle",
		Category:    "queue",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			if m.primaryBusy {
				if !m.interruptedThisTurn {
					m.interruptedThisTurn = true
					m.working.TaskDesc = "Interrupting…"
					slog.Debug("esc interrupt_turn", "component", "KEYMAP")
					if m.onCancel != nil {
						m.onCancel()
					}
					m.addSystemNotice("⏹ Interrupting current turn… (Esc)", false)
				}
				return nil, true
			}
			if m.inputBus != nil && m.inputBus.Len() > 0 {
				m.rollbackQueuedPrompt()
				return nil, true
			}
			if m.inputArea.Value() != "" {
				m.inputArea.Reset()
				m.resizeAllBuffers()
				return nil, true
			}
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "rollback_queue",
		Keys:        []string{"alt+up", "ctrl+up", "cmd+up", "super+up"},
		Label:       "Alt/Cmd+↑",
		Description: "Rollback latest unprocessed queued prompt into editor",
		Category:    "queue",
		When: func(m *MainUIModel) bool {
			return m.inputBus != nil && m.inputBus.Len() > 0
		},
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			m.rollbackQueuedPrompt()
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "squeeze_queue",
		Keys:        []string{"ctrl+e"},
		Label:       "Ctrl+E",
		Description: "Squeeze all pending queued prompts into one",
		Category:    "queue",
		When: func(m *MainUIModel) bool {
			return m.inputBus != nil && m.inputBus.Len() > 1
		},
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			n := m.inputBus.SqueezeInPlace()
			slog.Debug("manual_squeeze", "component", "INPUT_BUS", "merged", n)
			m.addSystemNotice(fmt.Sprintf("⚡ Squeezed %d queued commands into 1", n), false)
			m.resizeAllBuffers()
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "save_default_model",
		Keys:        []string{"ctrl+s"},
		Label:       "Ctrl+S",
		Description: "Save current model as global default",
		Category:    "session",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			if m.header.ModelName != "" {
				prov := m.header.ProviderName
				if prov == "" {
					prov = auth.InferProviderFromModel(m.header.ModelName)
				}
				full := m.header.ModelName
				if prov != "" && !strings.Contains(full, "/") {
					full = prov + "/" + full
				}
				m.applyModelSelection(auth.ModelEntry{
					Provider: prov,
					ID:       m.header.ModelName,
					FullName: full,
					BaseURL:  m.header.BaseURL,
				}, true)
			}
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "toggle_folds",
		Keys:        []string{"ctrl+o"},
		Label:       "ctrl+o",
		Description: "Toggle expand/collapse all tool & thinking folds",
		Category:    "view",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
				activeBuf.Transcript.ToggleAllExpanded()
				activeBuf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), false)
			}
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "toggle_thinking",
		Keys:        []string{"ctrl+t"},
		Label:       "ctrl+t",
		Description: "Toggle latest thinking block",
		Category:    "view",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
				activeBuf.Transcript.ToggleLatestThinking()
				activeBuf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), false)
			}
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "toggle_dag",
		Keys:        []string{"ctrl+g"},
		Label:       "ctrl+g",
		Description: "Toggle workflow DAG panel",
		Category:    "view",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			if len(m.workflowTree.nodes) > 0 {
				m.showDAG = !m.showDAG
				m.resizeAllBuffers()
			}
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "next_tab",
		Keys:        []string{"tab"},
		Label:       "Tab",
		Description: "Switch to next agent tab",
		Category:    "navigation",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			m.cycleNextTab()
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "prev_tab",
		Keys:        []string{"shift+tab"},
		Label:       "Shift+Tab",
		Description: "Switch to previous agent tab",
		Category:    "navigation",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			m.cyclePrevTab()
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "select_all",
		Keys:        []string{"ctrl+a"},
		Label:       "Ctrl+A",
		Description: "Select all text in active editor",
		Category:    "editor",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			if m.activeInputText() != "" {
				m.editorSelectedAll = true
			}
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "toggle_editor_zoom",
		Keys:        []string{"ctrl+x"},
		Label:       "Ctrl+X",
		Description: "Toggle expanded multi-line editor",
		Category:    "editor",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			m.toggleEditorZoom()
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "history_prev_ctrl",
		Keys:        []string{"ctrl+p"},
		Label:       "Ctrl+P",
		Description: "Recall previous input from history",
		Category:    "editor",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			m.navigateHistoryPrev()
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "history_next_ctrl",
		Keys:        []string{"ctrl+n"},
		Label:       "Ctrl+N",
		Description: "Move to next input in history",
		Category:    "editor",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			m.navigateHistoryNext()
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "submit_zoomed",
		Keys:        []string{"alt+enter", "ctrl+d"},
		Label:       "Alt+Enter / Ctrl+D",
		Description: "Submit prompt from zoomed multi-line editor",
		Category:    "editor",
		When: func(m *MainUIModel) bool {
			return m.editorZoomed
		},
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			m.submitActiveEditor()
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "submit_input",
		Keys:        []string{"enter"},
		Label:       "Enter",
		Description: "Submit prompt or local slash command",
		Category:    "editor",
		When: func(m *MainUIModel) bool {
			return !m.editorZoomed
		},
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			m.submitActiveEditor()
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "history_prev_up",
		Keys:        []string{"up"},
		Label:       "↑",
		Description: "Recall previous input when history exists",
		Category:    "editor",
		When: func(m *MainUIModel) bool {
			return !m.editorZoomed && m.inputHistory != nil && m.inputHistory.Len() > 0 &&
				len(MatchSlashCommands(m.inputArea.Value())) == 0
		},
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			m.navigateHistoryPrev()
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "history_next_down",
		Keys:        []string{"down"},
		Label:       "↓",
		Description: "Move to next input when browsing history",
		Category:    "editor",
		When: func(m *MainUIModel) bool {
			return !m.editorZoomed && m.inputHistory != nil && m.inputHistory.IsBrowsing() &&
				len(MatchSlashCommands(m.inputArea.Value())) == 0
		},
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			m.navigateHistoryNext()
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "scroll_up",
		Keys:        []string{"up"},
		Label:       "↑",
		Description: "Scroll transcript up 1 line",
		Category:    "navigation",
		When: func(m *MainUIModel) bool {
			return !m.editorZoomed
		},
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
				activeBuf.Scroll(-1)
				return nil, true
			}
			return nil, false
		},
	})

	km.Register(KeyBinding{
		ID:          "scroll_down",
		Keys:        []string{"down"},
		Label:       "↓",
		Description: "Scroll transcript down 1 line",
		Category:    "navigation",
		When: func(m *MainUIModel) bool {
			return !m.editorZoomed
		},
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
				activeBuf.Scroll(1)
				return nil, true
			}
			return nil, false
		},
	})

	km.Register(KeyBinding{
		ID:          "scroll_page_up",
		Keys:        []string{"pgup"},
		Label:       "PgUp",
		Description: "Scroll transcript up 5 lines",
		Category:    "navigation",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
				activeBuf.Scroll(-5)
				return nil, true
			}
			return nil, false
		},
	})

	km.Register(KeyBinding{
		ID:          "scroll_page_down",
		Keys:        []string{"pgdown"},
		Label:       "PgDown",
		Description: "Scroll transcript down 5 lines",
		Category:    "navigation",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			if activeBuf, ok := m.agentBuffers[m.activeTabID]; ok {
				activeBuf.Scroll(5)
				return nil, true
			}
			return nil, false
		},
	})
}
