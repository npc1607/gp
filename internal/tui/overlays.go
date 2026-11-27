package tui

import (
	"fmt"
	"strings"

	"gp/internal/ai"
	"gp/internal/core/auth"
	"gp/internal/core/storage"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// OverlayKind 表示当前激活的交互式弹层类型（对齐 Pi Selector / LoginDialog）
type OverlayKind int

const (
	OverlayNone OverlayKind = iota
	OverlayModelSelect
	OverlayLoginSelect
	OverlayLoginKey
	OverlayLogoutSelect
	OverlayResumeSelect
	OverlaySkillsSelect
	OverlayMCPSelect
	OverlayStatuslineSelect
	OverlayExtSelect
)

// OverlayState 管理 /model、/login、/logout、/skills、/mcp、/statusline、/ext 的交互式选择与输入状态
type OverlayState struct {
	Kind        OverlayKind
	Cursor      int
	SearchQuery string
	KeyInput    textinput.Model
	KeyInit     bool
	TargetProv  string
	Models      []auth.ModelEntry
	Providers   []auth.ProviderSpec
	LoggedInIDs []string
	Sessions    []storage.SessionSummary
	Skills          []SkillStatusItem
	MCPs            []MCPStatusItem
	Segments        []StatusSegment
	Extensions      []ExtensionStatusItem
	ThinkingLevel   string
	ThinkingOptions []ai.ThinkingOption
	StatusNote      string
}

func (o *OverlayState) syncThinkingOptionsForCursor(fallbackProvider, fallbackModel string) {
	prov := fallbackProvider
	model := fallbackModel
	filtered := o.filteredModels()
	if len(filtered) > 0 && o.Cursor >= 0 && o.Cursor < len(filtered) {
		prov = filtered[o.Cursor].Provider
		model = filtered[o.Cursor].FullName
		if model == "" {
			model = filtered[o.Cursor].ID
		}
	}
	prof := ai.ResolveThinkingProfile(nil, prov, model)
	o.ThinkingOptions = prof.Options
}

func cycleThinkingLevel(opts []ai.ThinkingOption, cur string, step int) string {
	if len(opts) == 0 {
		opts = ai.DefaultThinkingProfile().Options
	}
	norm := strings.ToLower(strings.TrimSpace(cur))
	idx := 0
	for i, opt := range opts {
		if strings.ToLower(opt.ID) == norm {
			idx = i
			break
		}
	}
	next := (idx + step + len(opts)) % len(opts)
	return opts[next].ID
}

func (o *OverlayState) InitKeyInput() {
	ti := textinput.New()
	ti.Prompt = "> "
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	ti.CharLimit = 512
	ti.Width = 64
	ti.Focus()
	o.KeyInput = ti
	o.KeyInit = true
}

func (o *OverlayState) Close() {
	o.Kind = OverlayNone
	o.Cursor = 0
	o.SearchQuery = ""
	if o.KeyInit {
		o.KeyInput.Reset()
	}
	o.TargetProv = ""
	o.StatusNote = ""
}

func (o *OverlayState) filteredModels() []auth.ModelEntry {
	q := strings.ToLower(strings.TrimSpace(o.SearchQuery))
	if q == "" {
		return o.Models
	}
	var out []auth.ModelEntry
	for _, m := range o.Models {
		if strings.Contains(strings.ToLower(m.FullName), q) ||
			strings.Contains(strings.ToLower(m.ID), q) ||
			strings.Contains(strings.ToLower(m.Provider), q) {
			out = append(out, m)
		}
	}
	return out
}

func (o *OverlayState) filteredSessions() []storage.SessionSummary {
	q := strings.ToLower(strings.TrimSpace(o.SearchQuery))
	if q == "" {
		return o.Sessions
	}
	var out []storage.SessionSummary
	for _, s := range o.Sessions {
		if strings.Contains(strings.ToLower(s.SessionID), q) ||
			strings.Contains(strings.ToLower(s.FirstPrompt), q) {
			out = append(out, s)
		}
	}
	return out
}

// RenderOverlay 渲染 pi / pi-zentui 风格的圆角选择器弹层
func RenderOverlay(o *OverlayState, width int, activeModel string, store *auth.Store, t Theme, cfg UIConfig) string {
	if o == nil || o.Kind == OverlayNone {
		return ""
	}
	boxW := max(min(width-4, 76), 36)
	innerW := boxW - 4

	var title string
	var lines []string

	switch o.Kind {
	case OverlayModelSelect:
		title = t.BoldFg(t.Accent, "Select Model")
		searchRow := t.Fg(t.Muted, "Search: ") + t.Fg(t.Text, o.SearchQuery) + t.BoldFg(t.Accent, "▌")
		lines = append(lines, ClipToWidth(searchRow, innerW, "…"))
		var thinkPills []string
		curThink := strings.ToLower(strings.TrimSpace(o.ThinkingLevel))
		if curThink == "" {
			curThink = "off"
		}
		opts := o.ThinkingOptions
		if len(opts) == 0 {
			opts = ai.DefaultThinkingProfile().Options
		}
		for _, opt := range opts {
			lbl := opt.DisplayLabel()
			if strings.ToLower(opt.ID) == curThink {
				thinkPills = append(thinkPills, t.BoldFg(t.Warning, "["+lbl+"]"))
			} else {
				thinkPills = append(thinkPills, t.Fg(t.Muted, lbl))
			}
		}
		thinkRow := t.Fg(t.Muted, "Thinking: ") + strings.Join(thinkPills, "  ") + t.Fg(t.Dim, "  (Tab / ←/→)")
		lines = append(lines, ClipToWidth(thinkRow, innerW, "…"))
		lines = append(lines, t.Fg(t.BorderMuted, strings.Repeat("─", innerW)))

		filtered := o.filteredModels()
		if len(filtered) == 0 {
			lines = append(lines, t.Fg(t.Muted, "  (no matching models — press Enter to use \""+o.SearchQuery+"\")"))
		} else {
			maxShow := 10
			start := 0
			if o.Cursor >= maxShow {
				start = o.Cursor - maxShow + 1
			}
			end := min(start+maxShow, len(filtered))
			for i := start; i < end; i++ {
				m := filtered[i]
				sel := "  "
				if i == o.Cursor {
					sel = t.BoldFg(t.Accent, "› ")
				}
				activeDot := " "
				if m.ID == activeModel || m.FullName == activeModel {
					activeDot = t.BoldFg(t.Success, "●")
				}
				badge := t.Fg(t.Dim, "○ not logged in")
				if m.Authenticated {
					badge = t.Fg(t.Success, "✓ "+m.AuthSource)
				}
				left := fmt.Sprintf("%s%s %s", sel, activeDot, t.Fg(t.Text, m.FullName))
				if i == o.Cursor {
					left = fmt.Sprintf("%s%s %s", sel, activeDot, t.BoldFg(t.Accent, m.FullName))
				}
				lines = append(lines, composeLeftRight(left, badge, innerW))
			}
		}
		lines = append(lines, t.Fg(t.BorderMuted, strings.Repeat("─", innerW)))
		lines = append(lines, t.Fg(t.Dim, "↑/↓ move · Tab/←/→ think · Enter switch · Ctrl+S save · Esc cancel"))

	case OverlayLoginSelect:
		title = t.BoldFg(t.Accent, "Login Provider")
		lines = append(lines, t.Fg(t.Muted, "Select a provider to authenticate (~/.gp/auth.db):"))
		lines = append(lines, "")
		for i, p := range o.Providers {
			sel := "  "
			name := t.Fg(t.Text, p.DisplayName)
			if i == o.Cursor {
				sel = t.BoldFg(t.Accent, "› ")
				name = t.BoldFg(t.Accent, p.DisplayName)
			}
			status := t.Fg(t.Dim, "○ not configured")
			if store != nil {
				if _, _, src, ok := store.ResolveCredential(p.ID); ok {
					status = t.Fg(t.Success, "✓ "+src)
				}
			}
			lines = append(lines, composeLeftRight(sel+name, status, innerW))
		}
		lines = append(lines, "")
		lines = append(lines, t.Fg(t.Dim, "↑/↓ move · Enter continue · Esc cancel"))

	case OverlayLoginKey:
		spec, _ := auth.FindProviderSpec(o.TargetProv)
		disp := o.TargetProv
		hint := "API Key"
		if spec.DisplayName != "" {
			disp = spec.DisplayName
		}
		if spec.KeyHint != "" {
			hint = spec.KeyHint
		}
		title = t.BoldFg(t.Accent, "Login · "+disp)
		lines = append(lines, t.Fg(t.Muted, "Enter API Key / Token ("+hint+") [optional: <key> <base_url>]:"))
		if !o.KeyInit {
			o.InitKeyInput()
		}
		lines = append(lines, o.KeyInput.View())
		if o.StatusNote != "" {
			lines = append(lines, t.Fg(t.Error, o.StatusNote))
		}
		lines = append(lines, "")
		lines = append(lines, t.Fg(t.Dim, "Enter save to ~/.gp/auth.db · Esc back"))

	case OverlayLogoutSelect:
		title = t.BoldFg(t.Warning, "Logout Provider")
		if len(o.LoggedInIDs) == 0 {
			lines = append(lines, t.Fg(t.Muted, "No credentials stored in ~/.gp/auth.db."))
		} else {
			lines = append(lines, t.Fg(t.Muted, "Select a provider to remove from ~/.gp/auth.db:"))
			lines = append(lines, "")
			for i, id := range o.LoggedInIDs {
				sel := "  "
				label := t.Fg(t.Text, id)
				if i == o.Cursor {
					sel = t.BoldFg(t.Warning, "› ")
					label = t.BoldFg(t.Warning, id)
				}
				if spec, ok := auth.FindProviderSpec(id); ok {
					label += t.Fg(t.Muted, " ("+spec.DisplayName+")")
				}
				lines = append(lines, sel+label)
			}
		}
		lines = append(lines, "")
		lines = append(lines, t.Fg(t.Dim, "↑/↓ move · Enter logout · Esc cancel"))

	case OverlayResumeSelect:
		title = t.BoldFg(t.Accent, "Resume Conversation")
		searchRow := t.Fg(t.Muted, "Search: ") + t.Fg(t.Text, o.SearchQuery) + t.BoldFg(t.Accent, "▌")
		lines = append(lines, ClipToWidth(searchRow, innerW, "…"))
		lines = append(lines, t.Fg(t.BorderMuted, strings.Repeat("─", innerW)))

		filtered := o.filteredSessions()
		if len(filtered) == 0 {
			lines = append(lines, t.Fg(t.Muted, "  (no past conversations found)"))
		} else {
			maxShow := 8
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
				prompt := s.FirstPrompt
				if prompt == "" {
					prompt = "(empty conversation)"
				}
				ts := ""
				if !s.UpdatedAt.IsZero() {
					ts = FormatMessageTime(s.UpdatedAt)
				}
				right := t.Fg(t.Muted, fmt.Sprintf("%d msgs · %s", s.MessageCount, ts))
				left := sel + t.Fg(t.Text, s.SessionID+" ") + t.Fg(t.Muted, prompt)
				if i == o.Cursor {
					left = sel + t.BoldFg(t.Accent, s.SessionID+" ") + t.Fg(t.Text, prompt)
				}
				lines = append(lines, composeLeftRight(left, right, innerW))
			}
		}
		lines = append(lines, t.Fg(t.BorderMuted, strings.Repeat("─", innerW)))
		lines = append(lines, t.Fg(t.Dim, "↑/↓ move · Enter resume · Esc cancel · type to filter"))

	case OverlaySkillsSelect:
		title, lines = renderSkillsOverlayLines(o, innerW, t)

	case OverlayMCPSelect:
		title, lines = renderMCPOverlayLines(o, innerW, t)

	case OverlayStatuslineSelect:
		title, lines = renderStatuslineOverlayLines(o, innerW, t)

	case OverlayExtSelect:
		title, lines = renderExtOverlayLines(o, innerW, t)
	}

	padded := make([]string, len(lines))
	for i, l := range lines {
		padded[i] = " " + l
	}
	framed := DrawFrame(padded, boxW, t, t.Accent, title, "left", 1)
	return strings.Join(framed, "\n")
}

// HandleOverlayKey 处理弹层激活时的键盘事件，返回是否已消费
func (m *MainUIModel) HandleOverlayKey(msg tea.KeyMsg) bool {
	if m.overlay == nil || m.overlay.Kind == OverlayNone {
		return false
	}
	key := msg.String()
	switch key {
	case "esc":
		m.overlay.Close()
		return true
	case "ctrl+c":
		return false
	}

	switch m.overlay.Kind {
	case OverlaySkillsSelect:
		return m.handleSkillsOverlayKey(msg)
	case OverlayMCPSelect:
		return m.handleMCPOverlayKey(msg)
	case OverlayStatuslineSelect:
		return m.handleStatuslineOverlayKey(msg)
	case OverlayExtSelect:
		return m.handleExtOverlayKey(msg)
	case OverlayModelSelect:
		filtered := m.overlay.filteredModels()
		switch key {
		case "up":
			if m.overlay.Cursor > 0 {
				m.overlay.Cursor--
				m.overlay.syncThinkingOptionsForCursor(m.header.ProviderName, m.header.ModelName)
			}
		case "down":
			if m.overlay.Cursor < len(filtered)-1 {
				m.overlay.Cursor++
				m.overlay.syncThinkingOptionsForCursor(m.header.ProviderName, m.header.ModelName)
			}
		case "tab", "right":
			next := cycleThinkingLevel(m.overlay.ThinkingOptions, m.overlay.ThinkingLevel, 1)
			m.overlay.ThinkingLevel = next
			m.applyThinkingSelection(next)
		case "shift+tab", "left":
			next := cycleThinkingLevel(m.overlay.ThinkingOptions, m.overlay.ThinkingLevel, -1)
			m.overlay.ThinkingLevel = next
			m.applyThinkingSelection(next)
		case "backspace":
			rs := []rune(m.overlay.SearchQuery)
			if len(rs) > 0 {
				m.overlay.SearchQuery = string(rs[:len(rs)-1])
				m.overlay.Cursor = 0
			}
		case "enter", "ctrl+s":
			persist := key == "ctrl+s"
			var chosen auth.ModelEntry
			if len(filtered) > 0 && m.overlay.Cursor >= 0 && m.overlay.Cursor < len(filtered) {
				chosen = filtered[m.overlay.Cursor]
			} else if q := strings.TrimSpace(m.overlay.SearchQuery); q != "" {
				prov := auth.InferProviderFromModel(q)
				chosen = auth.ModelEntry{Provider: prov, ID: q, FullName: q}
			}
			m.overlay.Close()
			if chosen.ID != "" {
				m.applyModelSelection(chosen, persist)
			}
		default:
			if msg.Type == tea.KeyRunes && len(msg.Runes) > 0 {
				m.overlay.SearchQuery += string(msg.Runes)
				m.overlay.Cursor = 0
			}
		}
		return true

	case OverlayLoginSelect:
		switch key {
		case "up":
			if m.overlay.Cursor > 0 {
				m.overlay.Cursor--
			}
		case "down":
			if m.overlay.Cursor < len(m.overlay.Providers)-1 {
				m.overlay.Cursor++
			}
		case "enter":
			if m.overlay.Cursor >= 0 && m.overlay.Cursor < len(m.overlay.Providers) {
				p := m.overlay.Providers[m.overlay.Cursor]
				m.overlay.Kind = OverlayLoginKey
				m.overlay.TargetProv = p.ID
				m.overlay.InitKeyInput()
				m.overlay.StatusNote = ""
			}
		}
		return true

	case OverlayLoginKey:
		if !m.overlay.KeyInit {
			m.overlay.InitKeyInput()
		}
		if key == "enter" {
			token := strings.TrimSpace(m.overlay.KeyInput.Value())
			if token == "" {
				m.overlay.StatusNote = "API key / token cannot be empty"
				return true
			}
			provID := m.overlay.TargetProv
			m.overlay.Close()
			m.applyLogin(provID, token)
			return true
		}
		m.overlay.KeyInput, _ = m.overlay.KeyInput.Update(msg)
		m.overlay.StatusNote = ""
		return true

	case OverlayLogoutSelect:
		switch key {
		case "up":
			if m.overlay.Cursor > 0 {
				m.overlay.Cursor--
			}
		case "down":
			if m.overlay.Cursor < len(m.overlay.LoggedInIDs)-1 {
				m.overlay.Cursor++
			}
		case "enter":
			if len(m.overlay.LoggedInIDs) == 0 {
				m.overlay.Close()
				return true
			}
			if m.overlay.Cursor >= 0 && m.overlay.Cursor < len(m.overlay.LoggedInIDs) {
				id := m.overlay.LoggedInIDs[m.overlay.Cursor]
				m.overlay.Close()
				m.applyLogout(id)
			}
		}
		return true

	case OverlayResumeSelect:
		filtered := m.overlay.filteredSessions()
		switch key {
		case "up":
			if m.overlay.Cursor > 0 {
				m.overlay.Cursor--
				m.overlay.syncThinkingOptionsForCursor(m.header.ProviderName, m.header.ModelName)
			}
		case "down":
			if m.overlay.Cursor < len(filtered)-1 {
				m.overlay.Cursor++
				m.overlay.syncThinkingOptionsForCursor(m.header.ProviderName, m.header.ModelName)
			}
		case "tab", "right":
			next := cycleThinkingLevel(m.overlay.ThinkingOptions, m.overlay.ThinkingLevel, 1)
			m.overlay.ThinkingLevel = next
			m.applyThinkingSelection(next)
		case "shift+tab", "left":
			next := cycleThinkingLevel(m.overlay.ThinkingOptions, m.overlay.ThinkingLevel, -1)
			m.overlay.ThinkingLevel = next
			m.applyThinkingSelection(next)
		case "backspace":
			rs := []rune(m.overlay.SearchQuery)
			if len(rs) > 0 {
				m.overlay.SearchQuery = string(rs[:len(rs)-1])
				m.overlay.Cursor = 0
			}
		case "enter":
			if len(filtered) > 0 && m.overlay.Cursor >= 0 && m.overlay.Cursor < len(filtered) {
				chosen := filtered[m.overlay.Cursor]
				m.overlay.Close()
				m.applyResumeSession(chosen.SessionID)
			}
		default:
			if msg.Type == tea.KeyRunes && len(msg.Runes) > 0 {
				m.overlay.SearchQuery += string(msg.Runes)
				m.overlay.Cursor = 0
			}
		}
		return true
	}
	return true
}
