package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestInputBus_QueueSqueezeAndRollback(t *testing.T) {
	bus := NewInputBus()
	if bus.Len() != 0 {
		t.Fatalf("initial Len() = %d, want 0", bus.Len())
	}

	bus.Enqueue("first task")
	bus.Enqueue("second task")
	bus.Enqueue("third task")
	if got := bus.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}

	// Rollback unprocessed latest item
	popped, ok := bus.RollbackLast()
	if !ok || popped.Text != "third task" {
		t.Fatalf("RollbackLast() = (%+v, %v), want 'third task'", popped, ok)
	}
	if got := bus.Len(); got != 2 {
		t.Fatalf("Len() after rollback = %d, want 2", got)
	}

	// Squeeze remaining 2 items in-place
	mergedCount := bus.SqueezeInPlace()
	if mergedCount != 2 {
		t.Fatalf("SqueezeInPlace() = %d, want 2", mergedCount)
	}
	if got := bus.Len(); got != 1 {
		t.Fatalf("Len() after SqueezeInPlace = %d, want 1", got)
	}

	// DequeueSqueezed returns the merged text
	item, n, ok := bus.DequeueSqueezed()
	if !ok || n != 1 || item.Text != "first task\n\nsecond task" {
		t.Fatalf("DequeueSqueezed() = (%q, %d, %v), want 'first task\\n\\nsecond task'", item.Text, n, ok)
	}
	if bus.Len() != 0 {
		t.Fatalf("Len() after DequeueSqueezed = %d, want 0", bus.Len())
	}
}

func TestTUI_EscInterruptsBusyAgentAndPreservesQueue(t *testing.T) {
	var submitted []string
	cancelled := 0
	m := createTestMainUIModel(t, func(prompt string) {
		submitted = append(submitted, prompt)
	})
	m.SetCancelHandler(func() {
		cancelled++
	})

	// 1. Submit first prompt -> dispatches immediately
	_, _ = m.Update(UserSubmitPromptMsg{Prompt: "initial prompt"})
	if len(submitted) != 1 || submitted[0] != "initial prompt" {
		t.Fatalf("submitted = %v, want [initial prompt]", submitted)
	}
	if !m.primaryBusy {
		t.Fatalf("expected primaryBusy=true after dispatch")
	}

	// 2. Submit two more prompts while busy -> queued on InputBus
	_, _ = m.Update(UserSubmitPromptMsg{Prompt: "follow up 1"})
	_, _ = m.Update(UserSubmitPromptMsg{Prompt: "follow up 2"})
	if len(submitted) != 1 {
		t.Fatalf("expected busy submit not to dispatch immediately, got %v", submitted)
	}
	if got := m.InputBus().Len(); got != 2 {
		t.Fatalf("InputBus.Len() = %d, want 2", got)
	}

	// 3. Alt+Up rolls back latest unprocessed item into editor
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp, Alt: true})
	if got := m.inputArea.Value(); got != "follow up 2" {
		t.Fatalf("inputArea.Value() after Alt+Up = %q, want 'follow up 2'", got)
	}
	if got := m.InputBus().Len(); got != 1 {
		t.Fatalf("InputBus.Len() after rollback = %d, want 1", got)
	}

	// 4. Esc while busy cancels active turn
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cancelled != 1 {
		t.Fatalf("cancelled = %d, want 1", cancelled)
	}

	// 5. Turn finishes as interrupted -> queued item is NOT auto-dispatched
	_, _ = m.Update(ActiveCoroutineMsg{AgentID: PrimaryAgentID, Active: false})
	if m.primaryBusy {
		t.Fatalf("expected primaryBusy=false after ActiveCoroutineMsg{Active: false}")
	}
	if len(submitted) != 1 {
		t.Fatalf("expected interrupted turn not to auto-dispatch queued items, got %v", submitted)
	}
	if got := m.InputBus().Len(); got != 1 {
		t.Fatalf("expected queued item to remain after interrupt, got Len()=%d", got)
	}
}

func TestTUI_QueuedPromptsSqueezeAndAutoDispatchOnCompletion(t *testing.T) {
	var submitted []string
	m := createTestMainUIModel(t, func(prompt string) {
		submitted = append(submitted, prompt)
	})

	_, _ = m.Update(UserSubmitPromptMsg{Prompt: "turn 1"})
	_, _ = m.Update(UserSubmitPromptMsg{Prompt: "part a"})
	_, _ = m.Update(UserSubmitPromptMsg{Prompt: "part b"})

	// Normal completion of turn 1 -> squeezes part a + part b into turn 2
	_, _ = m.Update(ActiveCoroutineMsg{AgentID: PrimaryAgentID, Active: false})
	if len(submitted) != 2 {
		t.Fatalf("submitted len = %d, want 2 (%v)", len(submitted), submitted)
	}
	if !strings.Contains(submitted[1], "part a") || !strings.Contains(submitted[1], "part b") {
		t.Fatalf("expected coalesced prompt containing both parts, got %q", submitted[1])
	}
	if m.InputBus().Len() != 0 {
		t.Fatalf("expected InputBus to be empty after auto-dispatch, got %d", m.InputBus().Len())
	}
}
