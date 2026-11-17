package tui

import (
	"fmt"
	"strings"
)

// SlashCommand 描述输入框支持补全与提示的斜杠指令
type SlashCommand struct {
	Name        string
	Usage       string
	Description string
}

// BuiltinSlashCommands 当前 TUI 支持的全部斜杠指令目录
var BuiltinSlashCommands = []SlashCommand{
	{Name: "/models", Usage: "/models [query|refresh]", Description: "选择、切换或设为默认启动模型（Ctrl+S 保存默认）"},
	{Name: "/login", Usage: "/login [qoder|deepseek]", Description: "登录并保存 Provider 凭证到 ~/.gp/auth.db"},
	{Name: "/logout", Usage: "/logout [provider]", Description: "注销并清除已保存的 Provider 凭证"},
	{Name: "/zentui", Usage: "/zentui preset opencode|rail|minimalist", Description: "实时切换编辑器、用户消息与状态栏样式预设"},
	{Name: "/tidy", Usage: "/tidy [turn]", Description: "开关 Turn 自动折叠或切换最近一轮折叠状态"},
	{Name: "/clear", Usage: "/clear", Description: "清空当前 Agent 会话缓冲区"},
}

// MatchSlashCommands 根据当前输入内容匹配可补全的斜杠指令
func MatchSlashCommands(input string) []SlashCommand {
	trimmed := strings.TrimLeft(input, " ")
	if !strings.HasPrefix(trimmed, "/") {
		return nil
	}
	// 一旦输入空格进入参数阶段，收起指令补全菜单
	if strings.Contains(trimmed, " ") {
		return nil
	}
	q := strings.ToLower(trimmed)
	var matches []SlashCommand
	for _, cmd := range BuiltinSlashCommands {
		if strings.HasPrefix(cmd.Name, q) {
			matches = append(matches, cmd)
		}
	}
	return matches
}

// RenderCompletionMenu 渲染 pi-zentui 风格的斜杠指令补全提示面板
func RenderCompletionMenu(matches []SlashCommand, cursor int, width int, t Theme) string {
	if len(matches) == 0 {
		return ""
	}
	if width < 24 {
		width = 24
	}
	maxShow := 6
	start := 0
	if cursor >= maxShow {
		start = cursor - maxShow + 1
	}
	end := start + maxShow
	if end > len(matches) {
		end = len(matches)
	}

	var rows []string
	for i := start; i < end; i++ {
		cmd := matches[i]
		prefix := "  "
		nameStr := t.Fg(t.Accent, fmt.Sprintf("%-10s", cmd.Name))
		if i == cursor {
			prefix = t.BoldFg(t.Accent, "› ")
			nameStr = t.BoldFg(t.Accent, fmt.Sprintf("%-10s", cmd.Name))
		}
		desc := t.Fg(t.Muted, cmd.Usage+"  —  "+cmd.Description)
		row := ClipToWidth(prefix+nameStr+" "+desc, width-2, "…")
		rows = append(rows, " "+row)
	}
	return strings.Join(rows, "\n")
}
