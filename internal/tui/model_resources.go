package tui

import (
	"fmt"
	"strings"
)

// SkillStatusItem 描述一个已发现 Skill 的展示与启用状态
type SkillStatusItem struct {
	Name        string
	Description string
	Source      string // "global" | "project" | "custom"
	Path        string
	Enabled     bool
}

// MCPStatusItem 描述一个已配置 MCP Server 的启用与连接状态
type MCPStatusItem struct {
	Name      string
	Command   string
	Args      []string
	Source    string // "global" | "project"
	Enabled   bool
	Connected bool
	ToolCount int
}

// ResourceHandlers 封装 /skills 与 /mcp 指令所需的查询与启停、取消注册回调
type ResourceHandlers struct {
	ListSkills      func() ([]SkillStatusItem, error)
	ToggleSkill     func(name string, enable bool) error
	AddSkillDir     func(dir string) error
	UnregisterSkill func(name string) error
	ListMCPs        func() ([]MCPStatusItem, error)
	ToggleMCP       func(name string, enable bool) error
	UnregisterMCP   func(name string) error
}

// SetResourceHandlers 注册 /skills 与 /mcp 指令处理器
func (m *MainUIModel) SetResourceHandlers(h ResourceHandlers) {
	m.resourceHandlers = h
}

func (m *MainUIModel) handleSkillsCommand(fields []string) bool {
	if len(fields) >= 3 {
		sub := strings.ToLower(fields[1])
		target := strings.TrimSpace(strings.Join(fields[2:], " "))
		switch sub {
		case "enable", "disable":
			if m.resourceHandlers.ToggleSkill == nil {
				m.addSystemNotice("Skill toggle is unavailable in current mode.", true)
				return true
			}
			enable := sub == "enable"
			if err := m.resourceHandlers.ToggleSkill(target, enable); err != nil {
				m.addSystemNotice(fmt.Sprintf("Skill %s failed: %v", sub, err), true)
				return true
			}
			state := "disabled"
			if enable {
				state = "enabled"
			}
			m.addSystemNotice(fmt.Sprintf("✓ Skill %q %s", target, state), false)
			return true
		case "add":
			if m.resourceHandlers.AddSkillDir == nil {
				m.addSystemNotice("Add skill dir is unavailable in current mode.", true)
				return true
			}
			if err := m.resourceHandlers.AddSkillDir(target); err != nil {
				m.addSystemNotice(fmt.Sprintf("Add skill dir failed: %v", err), true)
				return true
			}
			m.addSystemNotice(fmt.Sprintf("✓ Added skill directory: %s", target), false)
			return true
		case "remove", "unregister", "rm":
			if m.resourceHandlers.UnregisterSkill == nil {
				m.addSystemNotice("Skill unregister is unavailable in current mode.", true)
				return true
			}
			if err := m.resourceHandlers.UnregisterSkill(target); err != nil {
				m.addSystemNotice(fmt.Sprintf("Skill unregister failed: %v", err), true)
				return true
			}
			m.addSystemNotice(fmt.Sprintf("✓ Unregistered skill %q", target), false)
			return true
		}
	}

	if m.resourceHandlers.ListSkills == nil {
		m.addSystemNotice("Skill listing is unavailable in current mode.", true)
		return true
	}
	items, err := m.resourceHandlers.ListSkills()
	if err != nil {
		m.addSystemNotice(fmt.Sprintf("List skills failed: %v", err), true)
		return true
	}
	m.overlay.Close()
	m.overlay.Kind = OverlaySkillsSelect
	m.overlay.Skills = items
	return true
}

func (m *MainUIModel) handleMCPCommand(fields []string) bool {
	if len(fields) >= 3 {
		sub := strings.ToLower(fields[1])
		target := strings.TrimSpace(strings.Join(fields[2:], " "))
		switch sub {
		case "enable", "disable":
			if m.resourceHandlers.ToggleMCP == nil {
				m.addSystemNotice("MCP toggle is unavailable in current mode.", true)
				return true
			}
			enable := sub == "enable"
			if err := m.resourceHandlers.ToggleMCP(target, enable); err != nil {
				m.addSystemNotice(fmt.Sprintf("MCP %s failed: %v", sub, err), true)
				return true
			}
			state := "disabled"
			if enable {
				state = "enabled"
			}
			m.addSystemNotice(fmt.Sprintf("✓ MCP %q %s (takes effect on next restart)", target, state), false)
			return true
		case "remove", "unregister", "rm":
			if m.resourceHandlers.UnregisterMCP == nil {
				m.addSystemNotice("MCP unregister is unavailable in current mode.", true)
				return true
			}
			if err := m.resourceHandlers.UnregisterMCP(target); err != nil {
				m.addSystemNotice(fmt.Sprintf("MCP unregister failed: %v", err), true)
				return true
			}
			m.addSystemNotice(fmt.Sprintf("✓ Unregistered MCP %q", target), false)
			return true
		}
	}

	if m.resourceHandlers.ListMCPs == nil {
		m.addSystemNotice("MCP listing is unavailable in current mode.", true)
		return true
	}
	items, err := m.resourceHandlers.ListMCPs()
	if err != nil {
		m.addSystemNotice(fmt.Sprintf("List MCP servers failed: %v", err), true)
		return true
	}
	m.overlay.Close()
	m.overlay.Kind = OverlayMCPSelect
	m.overlay.MCPs = items
	return true
}
