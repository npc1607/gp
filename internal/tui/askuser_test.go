package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestAskUser_OpenEndedQuestionAllowsImmediateTyping(t *testing.T) {
	m := createTestMainUIModel(t, nil)
	respCh := make(chan AskUserResult, 1)

	_, _ = m.Update(AskUserQuestionMsg{
		Questions: []AskUserQuestion{
			{
				Question: "What should the file be named?",
				Header:   "Filename",
			},
		},
		ResponseCh: respCh,
	})

	if !m.askUser.EditingCustom {
		t.Fatalf("expected EditingCustom=true for 0-option question")
	}

	// Type "main.go" directly without pressing Enter first
	for _, r := range "main.go" {
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.askUser.Input.Value(); got != "main.go" {
		t.Fatalf("Input.Value() = %q, want 'main.go'", got)
	}

	// Press Enter to submit
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case res := <-respCh:
		if res.Cancelled || len(res.Answers) != 1 || res.Answers[0].Kind != "custom" || res.Answers[0].Answer != "main.go" {
			t.Fatalf("unexpected AskUserResult: %+v", res)
		}
	default:
		t.Fatalf("expected AskUserResult on respCh")
	}
}

func TestAskUser_CollapsedDoesNotBlockMainEditorAndNumberShortcutEntersCustom(t *testing.T) {
	m := createTestMainUIModel(t, nil)
	respCh := make(chan AskUserResult, 1)

	_, _ = m.Update(AskUserQuestionMsg{
		Questions: []AskUserQuestion{
			{
				Question: "Pick mode",
				Header:   "Mode",
				Options: []AskUserOption{
					{Label: "Fast"},
					{Label: "Safe"},
				},
			},
		},
		ResponseCh: respCh,
	})

	// Collapse via Ctrl+] -> main editor should accept input
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	if !m.askUser.Collapsed {
		t.Fatalf("expected Collapsed=true after Ctrl+]")
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if got := m.inputArea.Value(); got != "a" {
		t.Fatalf("expected main inputArea to receive 'a' when collapsed, got %q", got)
	}

	// Expand back via Ctrl+]
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlCloseBracket})
	if m.askUser.Collapsed {
		t.Fatalf("expected Collapsed=false after second Ctrl+]")
	}

	// Press '3' (Type something. index) -> enters EditingCustom
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	if !m.askUser.EditingCustom {
		t.Fatalf("expected EditingCustom=true after pressing '3' on 2-option question")
	}
	for _, r := range "custom" {
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := m.askUser.Input.Value(); got != "custom" {
		t.Fatalf("Input.Value() = %q, want 'custom'", got)
	}
}
