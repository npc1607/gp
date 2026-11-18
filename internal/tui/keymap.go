package tui

import (
	"fmt"
	"slices"
	"strings"
	"sync"

	"gp/internal/core/auth"

	tea "github.com/charmbracelet/bubbletea"
)

// KeyBinding 描述一条可注册、可查询、可动态重绑定的快捷键规则
type KeyBinding struct {
	ID          string
	Keys        []string
	Label       string
	Description string
	Category    string
	When        func(m *MainUIModel) bool
	Handler     func(m *MainUIModel) (tea.Cmd, bool)
}

// KeymapRegistry 线程安全的快捷键注册中心，支持绑定、重绑定、按按键/ID 查询与条件分发
type KeymapRegistry struct {
	mu       sync.RWMutex
	order    []string
	bindings map[string]KeyBinding
	byKey    map[string][]string
}

// NewKeymapRegistry 创建空的快捷键注册中心
func NewKeymapRegistry() *KeymapRegistry {
	return &KeymapRegistry{
		bindings: make(map[string]KeyBinding),
		byKey:    make(map[string][]string),
	}
}

// Register 注册或覆盖一条快捷键绑定（按 ID 去重并重建按键索引）
func (r *KeymapRegistry) Register(b KeyBinding) {
	if b.ID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if old, exists := r.bindings[b.ID]; exists {
		r.removeKeyIndexLocked(b.ID, old.Keys)
	} else {
		r.order = append(r.order, b.ID)
	}

	cp := b
	cp.Keys = slices.Clone(b.Keys)
	r.bindings[b.ID] = cp
	r.addKeyIndexLocked(b.ID, cp.Keys)
}

// Unregister 注销指定 ID 的快捷键绑定
func (r *KeymapRegistry) Unregister(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	old, exists := r.bindings[id]
	if !exists {
		return false
	}
	r.removeKeyIndexLocked(id, old.Keys)
	delete(r.bindings, id)
	r.order = slices.DeleteFunc(r.order, func(s string) bool { return s == id })
	return true
}

// Rebind 动态修改已有动作的触发按键与展示标签
func (r *KeymapRegistry) Rebind(id string, label string, keys ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	b, exists := r.bindings[id]
	if !exists {
		return false
	}
	r.removeKeyIndexLocked(id, b.Keys)
	b.Keys = slices.Clone(keys)
	if label != "" {
		b.Label = label
	}
	r.bindings[id] = b
	r.addKeyIndexLocked(id, b.Keys)
	return true
}

// Get 按动作 ID 查询绑定详情
func (r *KeymapRegistry) Get(id string) (KeyBinding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	b, ok := r.bindings[id]
	if !ok {
		return KeyBinding{}, false
	}
	b.Keys = slices.Clone(b.Keys)
	return b, true
}

// Lookup 按按键名查询所有关联的绑定
func (r *KeymapRegistry) Lookup(key string) []KeyBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := r.byKey[key]
	if len(ids) == 0 {
		return nil
	}
	out := make([]KeyBinding, 0, len(ids))
	for _, id := range ids {
		if b, ok := r.bindings[id]; ok {
			b.Keys = slices.Clone(b.Keys)
			out = append(out, b)
		}
	}
	return out
}

// List 按注册顺序返回全部快捷键绑定快照
func (r *KeymapRegistry) List() []KeyBinding {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]KeyBinding, 0, len(r.order))
	for _, id := range r.order {
		if b, ok := r.bindings[id]; ok {
			b.Keys = slices.Clone(b.Keys)
			out = append(out, b)
		}
	}
	return out
}

// HintLabel 查询指定动作用于 UI 提示的按键标签
func (r *KeymapRegistry) HintLabel(id, fallback string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	b, ok := r.bindings[id]
	if !ok {
		return fallback
	}
	if b.Label != "" {
		return b.Label
	}
	if len(b.Keys) > 0 {
		return strings.Join(b.Keys, "/")
	}
	return fallback
}

// Dispatch 根据按键匹配满足条件的绑定并执行处理函数
func (r *KeymapRegistry) Dispatch(key string, m *MainUIModel) (tea.Cmd, bool) {
	candidates := r.Lookup(key)
	for _, b := range candidates {
		if b.When != nil && !b.When(m) {
			continue
		}
		if b.Handler == nil {
			continue
		}
		if cmd, handled := b.Handler(m); handled {
			return cmd, true
		}
	}
	return nil, false
}

func (r *KeymapRegistry) addKeyIndexLocked(id string, keys []string) {
	for _, k := range keys {
		if k == "" {
			continue
		}
		if !slices.Contains(r.byKey[k], id) {
			r.byKey[k] = append(r.byKey[k], id)
		}
	}
}

func (r *KeymapRegistry) removeKeyIndexLocked(id string, keys []string) {
	for _, k := range keys {
		ids := r.byKey[k]
		if len(ids) == 0 {
			continue
		}
		filtered := slices.DeleteFunc(ids, func(s string) bool { return s == id })
		if len(filtered) == 0 {
			delete(r.byKey, k)
		} else {
			r.byKey[k] = filtered
		}
	}
}

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
				activeBuf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), false)
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
				activeBuf.SyncViewport(m.theme, m.config, m.working.SpinnerGlyph(m.config.WorkingSpinner), false)
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
		ID:          "submit_input",
		Keys:        []string{"enter"},
		Label:       "Enter",
		Description: "Submit prompt or local slash command",
		Category:    "editor",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) {
			val := strings.TrimSpace(m.inputArea.Value())
			if val != "" {
				m.inputArea.Reset()
				if m.handleLocalCommand(val) {
					return nil, true
				}
				m.submitUserPrompt(val)
			}
			return nil, true
		},
	})

	km.Register(KeyBinding{
		ID:          "scroll_up",
		Keys:        []string{"up"},
		Label:       "↑",
		Description: "Scroll transcript up 1 line",
		Category:    "navigation",
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
