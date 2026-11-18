package tui

import (
	"testing"

	"gp/internal/ai"
)

func TestTUI_ContextWindowResumeAndCompactCommand(t *testing.T) {
	m := NewMainUIModel(HeaderInfo{
		ModelName: "claude-4-sonnet",
	}, nil)

	if got, want := m.footer.ContextWindow, int64(200000); got != want {
		t.Fatalf("initial ContextWindow = %d, want %d", got, want)
	}

	// Simulate /resume restoring messages and ContextUsed
	m.SetSessionHandlers(nil, func(sessionID string) ([]ai.Message, error) {
		return []ai.Message{
			{Role: ai.RoleUser, Content: "Please inspect internal/tui/model.go and explain the event loop."},
			{Role: ai.RoleAssistant, Content: "The event loop uses Bubbletea Update/View with InputBus and KeymapRegistry."},
		}, nil
	})
	m.applyResumeSession("sess-123")
	if m.footer.ContextUsed <= 0 {
		t.Fatalf("expected ContextUsed > 0 after applyResumeSession, got %d", m.footer.ContextUsed)
	}

	// Simulate /compact command
	m.SetCompactHandler(func(focus string) (int64, int64, error) {
		return 50000, 8000, nil
	})
	if !m.handleLocalCommand("/compact keep api") {
		t.Fatalf("expected /compact to be handled")
	}
	if m.footer.ContextUsed != 8000 {
		t.Fatalf("expected ContextUsed = 8000 after /compact, got %d", m.footer.ContextUsed)
	}
}
