package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestRenderStartupBanner_CapsAtTop10AndWrapsInFrame(t *testing.T) {
	var toolsList []string
	var skillsList []SkillStatusItem
	var mcpList []MCPStatusItem
	for i := 1; i <= 13; i++ {
		toolsList = append(toolsList, fmt.Sprintf("tool_%02d", i))
		skillsList = append(skillsList, SkillStatusItem{Name: fmt.Sprintf("skill_%02d", i), Enabled: true})
		mcpList = append(mcpList, MCPStatusItem{Name: fmt.Sprintf("mcp_%02d", i), Enabled: true, Connected: true, ToolCount: 2})
	}

	banner := RenderStartupBanner(StartupSummary{
		Version:   "v0.1.0-alpha",
		ModelName: "qoder/qmodel_latest",
		Tools:     toolsList,
		Skills:    skillsList,
		MCPs:      mcpList,
	}, 80, DefaultTheme())

	if !strings.Contains(banner, "██████╗") {
		t.Fatalf("expected ASCII GP logo in banner, got:\n%s", banner)
	}
	for _, want := range []string{"Workspace Capabilities", "Tools  (13, top 10):", "Skills (13, top 10):", "MCP    (13, top 10):", "(+3 more)", "╭", "╰"} {
		if !strings.Contains(banner, want) {
			t.Fatalf("expected %q in banner, got:\n%s", want, banner)
		}
	}
	for line := range strings.SplitSeq(banner, "\n") {
		if w := VisibleWidth(line); w > 80 {
			t.Fatalf("line width %d exceeds max 80: %q", w, line)
		}
	}
	if strings.Contains(banner, "tool_11") || strings.Contains(banner, "skill_11") || strings.Contains(banner, "mcp_11") {
		t.Fatalf("expected items beyond top 10 to be omitted, got:\n%s", banner)
	}
}
