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
	{Name: "/resume", Usage: "/resume [session_id|search]", Description: "浏览并恢复历史会话 (Browse and resume past conversations)"},
	{Name: "/compact", Usage: "/compact [focus]", Description: "手动压缩上下文历史，释放上下文窗口空间"},
	{Name: "/skills", Usage: "/skills [enable|disable|remove|add <name|dir>]", Description: "查看已发现的 Skill（Space 启停，Ctrl+D 取消注册）"},
	{Name: "/mcp", Usage: "/mcp [enable|disable|remove <name>]", Description: "查看已配置的 MCP Server（Space 启停，Ctrl+D 取消注册）"},
	{Name: "/ext", Usage: "/ext [reload|enable|disable|test <name>]", Description: "统一管理外部工具能力包（Space 启停，Ctrl+T 探测连通性，Ctrl+R 热重载）"},
	{Name: "/login", Usage: "/login [qoder|deepseek]", Description: "登录并保存 Provider 凭证到 ~/.gp/auth.db"},
	{Name: "/logout", Usage: "/logout [provider]", Description: "注销并清除已保存的 Provider 凭证"},
	{Name: "/zentui", Usage: "/zentui preset opencode|rail|minimalist", Description: "实时切换编辑器、用户消息与状态栏样式预设"},
	{Name: "/statusline", Usage: "/statusline [list|toggle|on|off|reset] [id]", Description: "查看或开关底部状态栏已注册的系统内部上下文指标"},
	{Name: "/copy", Usage: "/copy [all]", Description: "复制最近一条助手回复（或 /copy all 复制全部对话）到剪贴板"},
	{Name: "/mouse", Usage: "/mouse [on|off]", Description: "开关 TUI 鼠标模式（关闭后使用终端原生选区）"},
	{Name: "/vim", Usage: "/vim [on|off]", Description: "开启或关闭 Vim 编辑模式（NORMAL/INSERT）"},
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
	end := min(start+maxShow, len(matches))

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
		row := ClipToWidth(prefix+nameStr+" "+desc, width-6, "…")
		rows = append(rows, row)
	}
	framed := DrawFrame(rows, width, t, t.BorderMuted, t.Fg(t.Muted, "Commands (Tab/Enter)"), "left", 1)
	return strings.Join(framed, "\n")
}
