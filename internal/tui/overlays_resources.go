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

func (o *OverlayState) filteredSegments() []StatusSegment {
	q := strings.ToLower(strings.TrimSpace(o.SearchQuery))
	if q == "" {
		return o.Segments
	}
	var out []StatusSegment
	for _, s := range o.Segments {
		if strings.Contains(strings.ToLower(s.ID), q) ||
			strings.Contains(strings.ToLower(s.Description), q) ||
			strings.Contains(strings.ToLower(s.Side), q) {
			out = append(out, s)
		}
	}
	return out
}

func renderStatuslineOverlayLines(o *OverlayState, innerW int, t Theme) (string, []string) {
	enabledCnt := 0
	for _, s := range o.Segments {
		if s.Enabled {
			enabledCnt++
		}
	}
	title := t.BoldFg(t.Accent, fmt.Sprintf("Statusline Segments (%d/%d on)", enabledCnt, len(o.Segments)))
	var lines []string
	searchRow := t.Fg(t.Muted, "Search: ") + t.Fg(t.Text, o.SearchQuery) + t.BoldFg(t.Accent, "▌")
	lines = append(lines, ClipToWidth(searchRow, innerW, "…"))
	lines = append(lines, t.Fg(t.BorderMuted, strings.Repeat("─", innerW)))

	filtered := o.filteredSegments()
	if len(filtered) == 0 {
		lines = append(lines, t.Fg(t.Muted, "  (no matching statusline segments)"))
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
			nameStr := t.Fg(t.Text, s.ID)
			if i == o.Cursor {
				nameStr = t.BoldFg(t.Accent, s.ID)
			}
			right := t.Fg(t.Muted, fmt.Sprintf("[%s · p%d]", s.Side, s.Priority))
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
	lines = append(lines, t.Fg(t.Dim, "↑/↓ move · Space toggle · Ctrl+R reset · Type to filter · Enter/Esc close"))
	return title, lines
}

func (m *MainUIModel) handleStatuslineOverlayKey(msg tea.KeyMsg) bool {
	filtered := m.overlay.filteredSegments()
	key := msg.String()
	switch key {
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
		if m.footer != nil && m.overlay.Cursor >= 0 && m.overlay.Cursor < len(filtered) {
			chosen := filtered[m.overlay.Cursor]
			en, ok := m.footer.ToggleSegment(chosen.ID)
			if ok {
				m.overlay.Segments = m.footer.ListSegments()
				state := "off"
				if en {
					state = "on"
				}
				m.overlay.StatusNote = fmt.Sprintf("Segment %q -> %s", chosen.ID, state)
			}
		}
	case "ctrl+r":
		if m.footer != nil {
			m.footer.ResetSegments()
			m.overlay.Segments = m.footer.ListSegments()
			m.overlay.StatusNote = "Reset all statusline segments to defaults"
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

func (o *OverlayState) filteredExtensions() []ExtensionStatusItem {
	q := strings.ToLower(strings.TrimSpace(o.SearchQuery))
	if q == "" {
		return o.Extensions
	}
	var out []ExtensionStatusItem
	for _, s := range o.Extensions {
		if strings.Contains(strings.ToLower(s.Name), q) ||
			strings.Contains(strings.ToLower(s.Description), q) ||
			strings.Contains(strings.ToLower(s.AdapterType), q) ||
			strings.Contains(strings.ToLower(s.Source), q) {
			out = append(out, s)
		}
	}
	return out
}

func renderExtOverlayLines(o *OverlayState, innerW int, t Theme) (string, []string) {
	enabledCnt := 0
	for _, s := range o.Extensions {
		if s.Enabled && s.State != "error" {
			enabledCnt++
		}
	}
	title := t.BoldFg(t.Accent, fmt.Sprintf("Extensions (%d/%d on)", enabledCnt, len(o.Extensions)))
	var lines []string
	searchRow := t.Fg(t.Muted, "Search: ") + t.Fg(t.Text, o.SearchQuery) + t.BoldFg(t.Accent, "▌")
	lines = append(lines, ClipToWidth(searchRow, innerW, "…"))
	lines = append(lines, t.Fg(t.BorderMuted, strings.Repeat("─", innerW)))

	filtered := o.filteredExtensions()
	if len(filtered) == 0 {
		lines = append(lines, t.Fg(t.Muted, "  (no extensions found in ~/.gp/extensions or .gp/extensions)"))
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
			if s.State == "error" {
				mark = t.Fg(t.Error, "[!] ")
			} else if s.Enabled {
				mark = t.Fg(t.Success, "[x] ")
			}
			nameStr := t.Fg(t.Text, s.Name)
			if i == o.Cursor {
				nameStr = t.BoldFg(t.Accent, s.Name)
			}
			meta := s.AdapterType
			if s.ActionCount > 0 {
				meta += fmt.Sprintf(" · %d acts", s.ActionCount)
			}
			if s.State != "" {
				meta += " · " + s.State
			}
			rightColor := t.Muted
			if s.State == "error" {
				rightColor = t.Error
			} else if s.State == "running" {
				rightColor = t.Success
			}
			right := t.Fg(rightColor, "["+meta+"]")
			left := sel + mark + nameStr
			if s.LoadError != "" {
				left += "  " + t.Fg(t.Error, s.LoadError)
			} else if desc := strings.TrimSpace(s.Description); desc != "" {
				left += "  " + t.Fg(t.Muted, desc)
			}
			lines = append(lines, composeLeftRight(left, right, innerW))
		}
	}
	if o.StatusNote != "" {
		lines = append(lines, t.Fg(t.Warning, ClipToWidth(o.StatusNote, innerW, "…")))
	}
	lines = append(lines, t.Fg(t.BorderMuted, strings.Repeat("─", innerW)))
	lines = append(lines, t.Fg(t.Dim, ClipToWidth("↑/↓ · Space toggle · ^T test · ^R reload · Type filter · Esc close", innerW, "…")))
	return title, lines
}

func (m *MainUIModel) handleExtOverlayKey(msg tea.KeyMsg) bool {
	filtered := m.overlay.filteredExtensions()
	key := msg.String()
	switch key {
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
			if chosen.State == "error" {
				m.overlay.StatusNote = fmt.Sprintf("Cannot enable %q: fix manifest error first", chosen.Name)
				return true
			}
			next := !chosen.Enabled
			if m.resourceHandlers.ToggleExtension != nil {
				if err := m.resourceHandlers.ToggleExtension(chosen.Name, next); err != nil {
					m.overlay.StatusNote = err.Error()
					return true
				}
			}
			if m.resourceHandlers.ListExtensions != nil {
				if updated, err := m.resourceHandlers.ListExtensions(); err == nil {
					m.overlay.Extensions = updated
					newFiltered := m.overlay.filteredExtensions()
					if m.overlay.Cursor >= len(newFiltered) && len(newFiltered) > 0 {
						m.overlay.Cursor = len(newFiltered) - 1
					} else if len(newFiltered) == 0 {
						m.overlay.Cursor = 0
					}
				}
			}
			state := "off"
			if next {
				state = "on"
			}
			m.overlay.StatusNote = fmt.Sprintf("Extension %q -> %s", chosen.Name, state)
		}
	case "ctrl+t":
		if m.resourceHandlers.TestExtension != nil && m.overlay.Cursor >= 0 && m.overlay.Cursor < len(filtered) {
			chosen := filtered[m.overlay.Cursor]
			if err := m.resourceHandlers.TestExtension(chosen.Name); err != nil {
				m.overlay.StatusNote = fmt.Sprintf("✗ %s: %v", chosen.Name, err)
			} else {
				m.overlay.StatusNote = fmt.Sprintf("✓ %s is healthy", chosen.Name)
			}
			// 探测可能触发 MCP 懒启动，刷新状态列表以即时反映 idle -> running
			if m.resourceHandlers.ListExtensions != nil {
				if updated, err := m.resourceHandlers.ListExtensions(); err == nil {
					m.overlay.Extensions = updated
				}
			}
		}
	case "ctrl+r":
		if m.resourceHandlers.ReloadExtensions != nil {
			if updated, err := m.resourceHandlers.ReloadExtensions(); err != nil {
				m.overlay.StatusNote = fmt.Sprintf("Reload failed: %v", err)
			} else {
				m.overlay.Extensions = updated
				m.overlay.Cursor = 0
				m.overlay.StatusNote = fmt.Sprintf("✓ Reloaded %d extension(s)", len(updated))
			}
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
