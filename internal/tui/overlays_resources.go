package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func (o *OverlayState) filteredSkills() []SkillStatusItem {
	q := strings.ToLower(strings.TrimSpace(o.SearchQuery))
	if q == "" {
		return o.Skills
	}
	var out []SkillStatusItem
	for _, s := range o.Skills {
		if strings.Contains(strings.ToLower(s.Name), q) ||
			strings.Contains(strings.ToLower(s.Description), q) ||
			strings.Contains(strings.ToLower(s.Source), q) {
			out = append(out, s)
		}
	}
	return out
}

func (o *OverlayState) filteredMCPs() []MCPStatusItem {
	q := strings.ToLower(strings.TrimSpace(o.SearchQuery))
	if q == "" {
		return o.MCPs
	}
	var out []MCPStatusItem
	for _, s := range o.MCPs {
		if strings.Contains(strings.ToLower(s.Name), q) ||
			strings.Contains(strings.ToLower(s.Command), q) ||
			strings.Contains(strings.ToLower(s.Source), q) {
			out = append(out, s)
		}
	}
	return out
}

func renderSkillsOverlayLines(o *OverlayState, innerW int, t Theme) (string, []string) {
	title := t.BoldFg(t.Accent, fmt.Sprintf("Skills (%d)", len(o.Skills)))
	var lines []string
	searchRow := t.Fg(t.Muted, "Search: ") + t.Fg(t.Text, o.SearchQuery) + t.BoldFg(t.Accent, "▌")
	lines = append(lines, ClipToWidth(searchRow, innerW, "…"))
	lines = append(lines, t.Fg(t.BorderMuted, strings.Repeat("─", innerW)))

	filtered := o.filteredSkills()
	if len(filtered) == 0 {
		lines = append(lines, t.Fg(t.Muted, "  (no skills found — use /skills add <dir>)"))
	} else {
		maxShow := 10
		start := 0
		if o.Cursor >= maxShow {
			start = o.Cursor - maxShow + 1
		}
		end := min(start+maxShow, len(filtered))
		for i := start; i < end; i++ {
			s := filtered[i]
			sel := "  "
			if i == o.Cursor {
				sel = t.BoldFg(t.Accent, "› ")
			}
			mark := t.Fg(t.Muted, "[ ] ")
			if s.Enabled {
				mark = t.Fg(t.Success, "[x] ")
			}
			nameStr := t.Fg(t.Text, s.Name)
			if i == o.Cursor {
				nameStr = t.BoldFg(t.Accent, s.Name)
			}
			right := t.Fg(t.Muted, "["+s.Source+"]")
			left := sel + mark + nameStr
			if desc := strings.TrimSpace(s.Description); desc != "" {
				left += "  " + t.Fg(t.Muted, desc)
			}
			lines = append(lines, composeLeftRight(left, right, innerW))
		}
	}
	if o.StatusNote != "" {
		lines = append(lines, t.Fg(t.Warning, ClipToWidth(o.StatusNote, innerW, "…")))
	}
	lines = append(lines, t.Fg(t.BorderMuted, strings.Repeat("─", innerW)))
	lines = append(lines, t.Fg(t.Dim, "↑/↓ move · Space toggle · Ctrl+D remove · Type to filter · Esc close"))
	return title, lines
}

func renderMCPOverlayLines(o *OverlayState, innerW int, t Theme) (string, []string) {
	title := t.BoldFg(t.Accent, fmt.Sprintf("MCP Servers (%d)", len(o.MCPs)))
	var lines []string
	searchRow := t.Fg(t.Muted, "Search: ") + t.Fg(t.Text, o.SearchQuery) + t.BoldFg(t.Accent, "▌")
	lines = append(lines, ClipToWidth(searchRow, innerW, "…"))
	lines = append(lines, t.Fg(t.BorderMuted, strings.Repeat("─", innerW)))

	filtered := o.filteredMCPs()
	if len(filtered) == 0 {
		lines = append(lines, t.Fg(t.Muted, "  (no MCP servers configured in ~/.gp/mcp.json or <cwd>/.gp/mcp.json)"))
	} else {
		maxShow := 10
		start := 0
		if o.Cursor >= maxShow {
			start = o.Cursor - maxShow + 1
		}
		end := min(start+maxShow, len(filtered))
		for i := start; i < end; i++ {
			s := filtered[i]
			sel := "  "
			if i == o.Cursor {
				sel = t.BoldFg(t.Accent, "› ")
			}
			mark := t.Fg(t.Muted, "[ ] ")
			if s.Enabled {
				mark = t.Fg(t.Success, "[x] ")
			}
			nameStr := t.Fg(t.Text, s.Name)
			if i == o.Cursor {
				nameStr = t.BoldFg(t.Accent, s.Name)
			}
			connStr := "offline"
			if s.Connected {
				connStr = fmt.Sprintf("connected·%d tools", s.ToolCount)
			}
			right := t.Fg(t.Muted, fmt.Sprintf("[%s | %s]", s.Source, connStr))
			cmdLine := strings.TrimSpace(s.Command + " " + strings.Join(s.Args, " "))
			left := sel + mark + nameStr + "  " + t.Fg(t.Muted, cmdLine)
			lines = append(lines, composeLeftRight(left, right, innerW))
		}
	}
	if o.StatusNote != "" {
		lines = append(lines, t.Fg(t.Warning, ClipToWidth(o.StatusNote, innerW, "…")))
	}
	lines = append(lines, t.Fg(t.BorderMuted, strings.Repeat("─", innerW)))
	lines = append(lines, t.Fg(t.Dim, "↑/↓ move · Space toggle · Ctrl+D remove · Type to filter · Esc close"))
	return title, lines
}

func (m *MainUIModel) handleSkillsOverlayKey(msg tea.KeyMsg) bool {
	filtered := m.overlay.filteredSkills()
	switch msg.String() {
	case "up":
		if m.overlay.Cursor > 0 {
			m.overlay.Cursor--
		}
	case "down":
		if m.overlay.Cursor < len(filtered)-1 {
			m.overlay.Cursor++
		}
	case "backspace":
		rs := []rune(m.overlay.SearchQuery)
		if len(rs) > 0 {
			m.overlay.SearchQuery = string(rs[:len(rs)-1])
			m.overlay.Cursor = 0
		}
	case " ":
		if m.overlay.Cursor >= 0 && m.overlay.Cursor < len(filtered) {
			chosen := filtered[m.overlay.Cursor]
			next := !chosen.Enabled
			if m.resourceHandlers.ToggleSkill != nil {
				if err := m.resourceHandlers.ToggleSkill(chosen.Name, next); err != nil {
					m.overlay.StatusNote = err.Error()
					return true
				}
			}
			for i := range m.overlay.Skills {
				if m.overlay.Skills[i].Name == chosen.Name {
					m.overlay.Skills[i].Enabled = next
				}
			}
			m.overlay.StatusNote = ""
		}
	case "ctrl+d", "delete":
		if m.overlay.Cursor >= 0 && m.overlay.Cursor < len(filtered) {
			chosen := filtered[m.overlay.Cursor]
			if m.resourceHandlers.UnregisterSkill != nil {
				if err := m.resourceHandlers.UnregisterSkill(chosen.Name); err != nil {
					m.overlay.StatusNote = err.Error()
					return true
				}
			}
			var remaining []SkillStatusItem
			for _, it := range m.overlay.Skills {
				if it.Name != chosen.Name {
					remaining = append(remaining, it)
				}
			}
			m.overlay.Skills = remaining
			if m.overlay.Cursor >= len(m.overlay.filteredSkills()) && m.overlay.Cursor > 0 {
				m.overlay.Cursor--
			}
			m.overlay.StatusNote = fmt.Sprintf("Unregistered skill %q", chosen.Name)
		}
	case "enter":
		m.overlay.Close()
	default:
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 0 {
			m.overlay.SearchQuery += string(msg.Runes)
			m.overlay.Cursor = 0
		}
	}
	return true
}

func (m *MainUIModel) handleMCPOverlayKey(msg tea.KeyMsg) bool {
	filtered := m.overlay.filteredMCPs()
	switch msg.String() {
	case "up":
		if m.overlay.Cursor > 0 {
			m.overlay.Cursor--
		}
	case "down":
		if m.overlay.Cursor < len(filtered)-1 {
			m.overlay.Cursor++
		}
	case "backspace":
		rs := []rune(m.overlay.SearchQuery)
		if len(rs) > 0 {
			m.overlay.SearchQuery = string(rs[:len(rs)-1])
			m.overlay.Cursor = 0
		}
	case " ":
		if m.overlay.Cursor >= 0 && m.overlay.Cursor < len(filtered) {
			chosen := filtered[m.overlay.Cursor]
			next := !chosen.Enabled
			if m.resourceHandlers.ToggleMCP != nil {
				if err := m.resourceHandlers.ToggleMCP(chosen.Name, next); err != nil {
					m.overlay.StatusNote = err.Error()
					return true
				}
			}
			for i := range m.overlay.MCPs {
				if m.overlay.MCPs[i].Name == chosen.Name {
					m.overlay.MCPs[i].Enabled = next
				}
			}
			m.overlay.StatusNote = "Saved (takes effect on next restart)"
		}
	case "ctrl+d", "delete":
		if m.overlay.Cursor >= 0 && m.overlay.Cursor < len(filtered) {
			chosen := filtered[m.overlay.Cursor]
			if m.resourceHandlers.UnregisterMCP != nil {
				if err := m.resourceHandlers.UnregisterMCP(chosen.Name); err != nil {
					m.overlay.StatusNote = err.Error()
					return true
				}
			}
			var remaining []MCPStatusItem
			for _, it := range m.overlay.MCPs {
				if it.Name != chosen.Name {
					remaining = append(remaining, it)
				}
			}
			m.overlay.MCPs = remaining
			if m.overlay.Cursor >= len(m.overlay.filteredMCPs()) && m.overlay.Cursor > 0 {
				m.overlay.Cursor--
			}
			m.overlay.StatusNote = fmt.Sprintf("Unregistered MCP %q", chosen.Name)
		}
	case "enter":
		m.overlay.Close()
	default:
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 0 {
			m.overlay.SearchQuery += string(msg.Runes)
			m.overlay.Cursor = 0
		}
	}
	return true
}
