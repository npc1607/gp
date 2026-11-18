package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestKeymapRegistry_RegisterQueryRebindUnregister(t *testing.T) {
	km := NewDefaultKeymap()

	// Query by ID
	b, ok := km.Get("rollback_queue")
	if !ok {
		t.Fatalf("Get(rollback_queue) = false, want true")
	}
	if b.Label != "Alt/Cmd+↑" {
		t.Fatalf("rollback_queue Label = %q, want 'Alt/Cmd+↑'", b.Label)
	}

	// Query by key
	matches := km.Lookup("cmd+up")
	if len(matches) != 1 || matches[0].ID != "rollback_queue" {
		t.Fatalf("Lookup(cmd+up) = %+v, want [rollback_queue]", matches)
	}

	// List all bindings
	all := km.List()
	if len(all) < 10 {
		t.Fatalf("List() len = %d, want >= 10", len(all))
	}

	// Dynamic rebind
	if !km.Rebind("squeeze_queue", "Ctrl+J", "ctrl+j") {
		t.Fatalf("Rebind(squeeze_queue) = false, want true")
	}
	if got := km.HintLabel("squeeze_queue", ""); got != "Ctrl+J" {
		t.Fatalf("HintLabel(squeeze_queue) = %q, want 'Ctrl+J'", got)
	}
	if old := km.Lookup("ctrl+e"); len(old) != 0 {
		t.Fatalf("expected old key ctrl+e to be unbound, got %+v", old)
	}
	if nw := km.Lookup("ctrl+j"); len(nw) != 1 || nw[0].ID != "squeeze_queue" {
		t.Fatalf("Lookup(ctrl+j) = %+v, want [squeeze_queue]", nw)
	}

	// Unregister custom binding
	km.Register(KeyBinding{
		ID:      "custom_ping",
		Keys:    []string{"f12"},
		Label:   "F12",
		Handler: func(m *MainUIModel) (tea.Cmd, bool) { return nil, true },
	})
	if len(km.Lookup("f12")) != 1 {
		t.Fatalf("expected f12 to be registered")
	}
	if !km.Unregister("custom_ping") {
		t.Fatalf("Unregister(custom_ping) = false, want true")
	}
	if len(km.Lookup("f12")) != 0 {
		t.Fatalf("expected f12 to be removed after Unregister")
	}
}

func TestKeymapRegistry_RebindDispatchInMainUI(t *testing.T) {
	m := createTestMainUIModel(t, func(string) {})
	_, _ = m.Update(UserSubmitPromptMsg{Prompt: "turn 1"})
	_, _ = m.Update(UserSubmitPromptMsg{Prompt: "queued 1"})
	_, _ = m.Update(UserSubmitPromptMsg{Prompt: "queued 2"})

	// Rebind squeeze_queue from ctrl+e to ctrl+y
	if !m.Keymap().Rebind("squeeze_queue", "Ctrl+Y", "ctrl+y") {
		t.Fatalf("Rebind failed")
	}
	// Old key should not squeeze
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlE})
	if got := m.InputBus().Len(); got != 2 {
		t.Fatalf("expected old key ctrl+e not to squeeze, Len() = %d, want 2", got)
	}
	// New key should squeeze
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlY})
	if got := m.InputBus().Len(); got != 1 {
		t.Fatalf("expected rebound key ctrl+y to squeeze to 1, Len() = %d", got)
	}
}
