package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestTUI_SkillsAndMCPOverlaySpaceToggle(t *testing.T) {
	m := createTestMainUIModel(t, nil)

	skillEnabled := true
	skillRemoved := false
	var addedDir string
	mcpEnabled := true
	mcpRemoved := false

	m.SetResourceHandlers(ResourceHandlers{
		ListSkills: func() ([]SkillStatusItem, error) {
			if skillRemoved {
				return nil, nil
			}
			return []SkillStatusItem{
				{
					Name:        "git-commit",
					Description: "Generate conventional commit messages",
					Source:      "custom",
					Path:        "/Users/pcong/Project/services/data-service/.agents/skills/git-commit/SKILL.md",
					Enabled:     skillEnabled,
				},
			}, nil
		},
		ToggleSkill: func(name string, enable bool) error {
			if name == "git-commit" {
				skillEnabled = enable
			}
			return nil
		},
		AddSkillDir: func(dir string) error {
			addedDir = dir
			return nil
		},
		UnregisterSkill: func(name string) error {
			if name == "git-commit" {
				skillRemoved = true
			}
			return nil
		},
		ListMCPs: func() ([]MCPStatusItem, error) {
			if mcpRemoved {
				return nil, nil
			}
			return []MCPStatusItem{
				{
					Name:      "git",
					Command:   "uvx",
					Args:      []string{"mcp-server-git"},
					Source:    "global",
					Enabled:   mcpEnabled,
					Connected: mcpEnabled,
					ToolCount: 3,
				},
			}, nil
		},
		ToggleMCP: func(name string, enable bool) error {
			if name == "git" {
				mcpEnabled = enable
			}
			return nil
		},
		UnregisterMCP: func(name string) error {
			if name == "git" {
				mcpRemoved = true
			}
			return nil
		},
	})

	if !m.handleLocalCommand("/skills add /tmp/custom-skills") || addedDir != "/tmp/custom-skills" {
		t.Fatalf("expected /skills add to record dir, got %q", addedDir)
	}

	// Open /skills overlay
	if !m.handleLocalCommand("/skills") || m.overlay.Kind != OverlaySkillsSelect {
		t.Fatalf("expected /skills to open OverlaySkillsSelect, got %v", m.overlay.Kind)
	}
	if !strings.Contains(m.View(), "[x] git-commit") {
		t.Fatalf("expected '[x] git-commit' in skills overlay view:\n%s", m.View())
	}

	// Press Space to disable
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	if skillEnabled {
		t.Fatalf("expected Space to disable git-commit")
	}
	if !strings.Contains(m.View(), "[ ] git-commit") {
		t.Fatalf("expected '[ ] git-commit' after Space toggle:\n%s", m.View())
	}

	// Press Esc to close
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.overlay.Kind != OverlayNone {
		t.Fatalf("expected Esc to close overlay, got %v", m.overlay.Kind)
	}

	// Open /mcp overlay and toggle with Space
	if !m.handleLocalCommand("/mcp") || m.overlay.Kind != OverlayMCPSelect {
		t.Fatalf("expected /mcp to open OverlayMCPSelect, got %v", m.overlay.Kind)
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	if mcpEnabled {
		t.Fatalf("expected Space to disable git mcp")
	}
	if !strings.Contains(m.View(), "[ ] git") {
		t.Fatalf("expected '[ ] git' after Space toggle:\n%s", m.View())
	}

	// Press Ctrl+D to unregister MCP
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if !mcpRemoved || len(m.overlay.MCPs) != 0 {
		t.Fatalf("expected Ctrl+D to unregister git MCP")
	}
	_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})

	// Command /skills remove git-commit
	if !m.handleLocalCommand("/skills remove git-commit") || !skillRemoved {
		t.Fatalf("expected /skills remove git-commit to unregister skill")
	}
}
