package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestEditor_VimModeToggleAndMotions(t *testing.T) {
	m := createTestMainUIModel(t, nil)

	if !m.handleLocalCommand("/vim on") {
		t.Fatalf("expected /vim on to be handled")
	}
	if !m.vim.Enabled || m.vim.Mode != VimInsert {
		t.Fatalf("expected vim enabled in INSERT mode, got enabled=%v mode=%s", m.vim.Enabled, m.vim.Mode)
	}

	// Type "hello" in INSERT mode
	for _, r := range "hello" {
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	// Esc switches to NORMAL mode
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.vim.Mode != VimNormal {
		t.Fatalf("expected NORMAL mode after Esc, got %s", m.vim.Mode)
	}
	if !strings.Contains(m.View(), "-- NORMAL --") {
		t.Fatalf("expected '-- NORMAL --' badge in view")
	}

	// 'dd' clears the line
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if got := m.inputArea.Value(); got != "" {
		t.Fatalf("expected 'dd' to clear input, got %q", got)
	}

	// 'i' returns to INSERT mode
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	if m.vim.Mode != VimInsert {
		t.Fatalf("expected INSERT mode after 'i', got %s", m.vim.Mode)
	}

	// /vim off disables Vim mode
	_, _ = m.handleLocalCommand("/vim off"), true
	if m.vim.Enabled {
		t.Fatalf("expected /vim off to disable vim mode")
	}
}

func TestEditor_ZoomAndInputHistory(t *testing.T) {
	var submitted []string
	m := createTestMainUIModel(t, func(p string) {
		submitted = append(submitted, p)
	})

	// Submit two prompts to populate history
	m.inputArea.SetValue("first prompt")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, _ = m.Update(ActiveCoroutineMsg{AgentID: PrimaryAgentID, Active: false})

	m.inputArea.SetValue("second prompt")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_, _ = m.Update(ActiveCoroutineMsg{AgentID: PrimaryAgentID, Active: false})

	// Type a draft, then Up -> second prompt -> Up -> first prompt -> Down -> second prompt -> Down -> draft
	m.inputArea.SetValue("my draft")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.inputArea.Value(); got != "second prompt" {
		t.Fatalf("Up #1 = %q, want 'second prompt'", got)
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.inputArea.Value(); got != "first prompt" {
		t.Fatalf("Up #2 = %q, want 'first prompt'", got)
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := m.inputArea.Value(); got != "second prompt" {
		t.Fatalf("Down #1 = %q, want 'second prompt'", got)
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := m.inputArea.Value(); got != "my draft" {
		t.Fatalf("Down #2 = %q, want 'my draft'", got)
	}

	// Ctrl+X toggles zoomed multi-line editor and preserves text
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	if !m.editorZoomed {
		t.Fatalf("expected editorZoomed=true after Ctrl+X")
	}
	if !strings.Contains(m.View(), "Zoomed Editor") {
		t.Fatalf("expected 'Zoomed Editor' in view")
	}
	if got := m.zoomArea.Value(); got != "my draft" {
		t.Fatalf("zoomArea.Value() = %q, want 'my draft'", got)
	}

	// Alt+Enter submits from zoomed editor
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	if len(submitted) != 3 || submitted[2] != "my draft" {
		t.Fatalf("expected Alt+Enter to submit 'my draft', got %v", submitted)
	}

	// Toggle back to compact editor, then Select All (Ctrl+A) + replace by typing
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	m.inputArea.SetValue("old text")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlA})
	if !m.editorSelectedAll {
		t.Fatalf("expected editorSelectedAll=true after Ctrl+A")
	}
	if !strings.Contains(m.View(), "[ALL]") {
		t.Fatalf("expected '[ALL]' badge in view")
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if got := m.inputArea.Value(); got != "n" {
		t.Fatalf("expected typing after Ctrl+A to replace text with 'n', got %q", got)
	}

	// Zoomed multi-line editor in Vim INSERT mode: Up/Down moves cursor inside zoomArea (does not scroll transcript)
	m.handleLocalCommand("/vim on")
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlX})
	m.zoomArea.SetValue("line1\nline2")
	initLine := m.zoomArea.Line()
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.zoomArea.Line() >= initLine {
		t.Fatalf("expected Up in zoomed Vim INSERT mode to move cursor up from line %d, got %d", initLine, m.zoomArea.Line())
	}
}
