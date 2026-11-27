package tui

import (
	"fmt"
	"sort"
	"strings"
)

// StartupSummary 汇总启动欢迎页展示的 ASCII Logo、工具、Skills 与 MCP 摘要
type StartupSummary struct {
	Version   string
	ModelName string
	Tools     []string
	Skills    []SkillStatusItem
	MCPs      []MCPStatusItem
}

const gpASCIIBanner = `
  ██████╗ ██████╗ 
 ██╔════╝ ██╔══██╗
 ██║  ███╗██████╔╝
 ██║   ██║██╔═══╝ 
 ╚██████╔╝██║     
  ╚═════╝ ╚═╝     `

// RenderStartupBanner 生成带大号 ASCII GP Logo 以及圆角边框包裹的前 10 项 Tools / Skills / MCP 欢迎面板
func RenderStartupBanner(s StartupSummary, width int, t Theme) string {
	if width <= 0 {
		width = 88
	}
	boxW := max(min(width-4, 92), 44)
	innerW := boxW - 4

	var sb strings.Builder
	sb.WriteString(strings.TrimPrefix(gpASCIIBanner, "\n"))
	ver := strings.TrimSpace(s.Version)
	if ver == "" {
		ver = "v0.1.0-alpha"
	}
	fmt.Fprintf(&sb, " %s", ver)
	if strings.TrimSpace(s.ModelName) != "" {
		fmt.Fprintf(&sb, " · %s", s.ModelName)
	}
	sb.WriteString("\n\n")

	skillLabels := make([]string, 0, len(s.Skills))
	for _, sk := range s.Skills {
		state := "on"
		if !sk.Enabled {
			state = "off"
		}
		skillLabels = append(skillLabels, fmt.Sprintf("%s(%s)", sk.Name, state))
	}

	mcpLabels := make([]string, 0, len(s.MCPs))
	for _, m := range s.MCPs {
		state := "off"
		if m.Connected {
			state = fmt.Sprintf("connected:%d", m.ToolCount)
		} else if m.Enabled {
			state = "on"
		}
		mcpLabels = append(mcpLabels, fmt.Sprintf("%s(%s)", m.Name, state))
	}

	var boxLines []string
	boxLines = append(boxLines, wrapTopList("Tools", s.Tools, 10, innerW)...)
	boxLines = append(boxLines, wrapTopList("Skills", skillLabels, 10, innerW)...)
	boxLines = append(boxLines, wrapTopList("MCP", mcpLabels, 10, innerW)...)

	framed := DrawFrame(boxLines, boxW, t, t.BorderMuted, t.BoldFg(t.Accent, "Workspace Capabilities"), "left", 1)
	sb.WriteString(strings.Join(framed, "\n"))
	return sb.String()
}

func wrapTopList(label string, items []string, limit, maxW int) []string {
	if len(items) == 0 {
		return []string{fmt.Sprintf("%-6s (0): none", label)}
	}
	sorted := make([]string, len(items))
	copy(sorted, items)
	sort.Strings(sorted)

	total := len(sorted)
	suffix := ""
	shown := sorted
	if total > limit {
		shown = sorted[:limit]
		suffix = fmt.Sprintf(" … (+%d more)", total-limit)
	}

	header := fmt.Sprintf("%-6s (%d): ", label, total)
	if total > limit {
		header = fmt.Sprintf("%-6s (%d, top %d): ", label, total, limit)
	}
	indent := strings.Repeat(" ", len(header))

	var lines []string
	cur := header
	for i, item := range shown {
		sep := ", "
		if i == 0 {
			sep = ""
		}
		cand := cur + sep + item
		if i == len(shown)-1 {
			cand += suffix
		}
		if VisibleWidth(cand) > maxW && cur != header {
			lines = append(lines, cur+",")
			cur = indent + item
		} else {
			cur += sep + item
		}
	}
	if suffix != "" {
		if VisibleWidth(cur+suffix) > maxW && cur != indent {
			lines = append(lines, cur)
			lines = append(lines, indent+strings.TrimSpace(suffix))
		} else {
			cur += suffix
			lines = append(lines, cur)
		}
	} else {
		lines = append(lines, cur)
	}
	return lines
}

// ShowStartupBanner 将启动欢迎面板写入主会话视图（不参与模型上下文）
func (m *MainUIModel) ShowStartupBanner(s StartupSummary) {
	if buf, ok := m.agentBuffers[PrimaryAgentID]; ok {
		buf.Transcript.AddNotice(RenderStartupBanner(s, m.width, m.theme))
		buf.SyncViewport(m.theme, m.config, m.transcriptPulseGlyph(), true)
	}
}
