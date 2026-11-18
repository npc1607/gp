package tui

import (
	"slices"
	"strings"
	"sync"

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
