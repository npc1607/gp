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

// ExtensionStatusItem 描述一个统一 Extension 能力包的展示与运行状态
type ExtensionStatusItem struct {
	Name        string
	Description string
	AdapterType string // guide | cli | script | http | openapi | mcp
	Source      string // global | custom | project | legacy-skill | legacy-mcp
	Enabled     bool
	ActionCount int
	State       string // ready | running | idle | disabled | error
	LoadError   string
}

// ResourceHandlers 封装 /skills、/mcp 与 /ext 指令所需的查询、启停与热重载回调
type ResourceHandlers struct {
	ListSkills       func() ([]SkillStatusItem, error)
	ToggleSkill      func(name string, enable bool) error
	AddSkillDir      func(dir string) error
	UnregisterSkill  func(name string) error
	ListMCPs         func() ([]MCPStatusItem, error)
	ToggleMCP        func(name string, enable bool) error
	UnregisterMCP    func(name string) error
	ListExtensions   func() ([]ExtensionStatusItem, error)
	ToggleExtension  func(name string, enable bool) error
	ReloadExtensions func() ([]ExtensionStatusItem, error)
	TestExtension    func(name string) error
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

func (m *MainUIModel) handleExtCommand(fields []string) bool {
	if len(fields) >= 2 {
		sub := strings.ToLower(fields[1])
		switch sub {
		case "reload":
			if m.resourceHandlers.ReloadExtensions == nil {
				m.addSystemNotice("Extension reload is unavailable in current mode.", true)
				return true
			}
			items, err := m.resourceHandlers.ReloadExtensions()
			if err != nil {
				m.addSystemNotice(fmt.Sprintf("Reload extensions failed: %v", err), true)
				return true
			}
			m.addSystemNotice(fmt.Sprintf("✓ Reloaded %d extension(s)", len(items)), false)
			return true

		case "enable", "disable":
			if len(fields) < 3 {
				m.addSystemNotice(fmt.Sprintf("Usage: /ext %s <name>", sub), true)
				return true
			}
			if m.resourceHandlers.ToggleExtension == nil {
				m.addSystemNotice("Extension toggle is unavailable in current mode.", true)
				return true
			}
			target := strings.TrimSpace(strings.Join(fields[2:], " "))
			enable := sub == "enable"
			if err := m.resourceHandlers.ToggleExtension(target, enable); err != nil {
				m.addSystemNotice(fmt.Sprintf("Extension %s failed: %v", sub, err), true)
				return true
			}
			state := "disabled"
			if enable {
				state = "enabled"
			}
			m.addSystemNotice(fmt.Sprintf("✓ Extension %q %s", target, state), false)
			return true

		case "test", "check":
			if len(fields) < 3 {
				m.addSystemNotice("Usage: /ext test <name>", true)
				return true
			}
			if m.resourceHandlers.TestExtension == nil {
				m.addSystemNotice("Extension healthcheck is unavailable in current mode.", true)
				return true
			}
			target := strings.TrimSpace(strings.Join(fields[2:], " "))
			if err := m.resourceHandlers.TestExtension(target); err != nil {
				m.addSystemNotice(fmt.Sprintf("✗ Extension %q healthcheck failed: %v", target, err), true)
				return true
			}
			m.addSystemNotice(fmt.Sprintf("✓ Extension %q is healthy", target), false)
			return true
		}
	}

	if m.resourceHandlers.ListExtensions == nil {
		m.addSystemNotice("Extension listing is unavailable in current mode.", true)
		return true
	}
	items, err := m.resourceHandlers.ListExtensions()
	if err != nil {
		m.addSystemNotice(fmt.Sprintf("List extensions failed: %v", err), true)
		return true
	}
	m.overlay.Close()
	m.overlay.Kind = OverlayExtSelect
	m.overlay.Extensions = items
	return true
}
