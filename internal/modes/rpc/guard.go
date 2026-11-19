package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gp/internal/agent"
	"gp/internal/ai"
	"gp/internal/tools"
)

// riskVerdict represents the structured decision returned by the security review subagent.
type riskVerdict struct {
	RiskLevel          string `json:"risk_level"` // "high" | "medium" | "low"
	Allow              bool   `json:"allow"`
	Reason             string `json:"reason"`
	RequiredPermission string `json:"required_permission"`
}

// riskGuard evaluates potentially dangerous tool invocations using a dedicated security subagent.
type riskGuard struct {
	prov       ai.Provider
	modelName  string
	userPrompt string
	reqID      string
	emit       func(Event)
}

func wrapRegistryWithRiskGuard(reg *tools.Registry, guard *riskGuard) *tools.Registry {
	if reg == nil || guard == nil {
		return reg
	}
	guarded := tools.NewRegistry()
	for _, t := range reg.List() {
		if isSensitiveTool(t.Name()) {
			_ = guarded.Register(&guardedTool{inner: t, guard: guard})
		} else {
			_ = guarded.Register(t)
		}
	}
	return guarded
}

func isSensitiveTool(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "read", "grep", "find", "ls", "subagent", "ask_peer_agent":
		return false
	case "bash", "write", "edit", "codemode":
		return true
	}
	// Any read-only discovery/read MCP tool passes; all mutating/remote/exec tools go through audit.
	if strings.Contains(lower, "read") || strings.Contains(lower, "resolve") || strings.Contains(lower, "list") || strings.Contains(lower, "get") || strings.Contains(lower, "search") {
		if !strings.Contains(lower, "delete") && !strings.Contains(lower, "remove") && !strings.Contains(lower, "update") && !strings.Contains(lower, "edit") {
			return false
		}
	}
	return true
}

type guardedTool struct {
	inner tools.Tool
	guard *riskGuard
}

func (g *guardedTool) Name() string                 { return g.inner.Name() }
func (g *guardedTool) Description() string          { return g.inner.Description() }
func (g *guardedTool) InputSchema() json.RawMessage { return g.inner.InputSchema() }

func (g *guardedTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	if g.guard.shouldSkipCheck(g.inner.Name(), rawArgs) {
		return g.inner.Execute(ctx, rawArgs)
	}

	verdict, err := g.guard.assess(ctx.Context, g.inner.Name(), rawArgs)
	if err != nil {
		// Fail-closed if assessment fails on a potentially destructive operation
		return nil, fmt.Errorf("安全审计子 Agent 评估失败，基于安全原则拒绝执行 %s 操作 (%v)；需要更高级别权限或人工确认", g.inner.Name(), err)
	}

	if !verdict.Allow || strings.EqualFold(verdict.RiskLevel, "high") {
		permHint := verdict.RequiredPermission
		if permHint == "" {
			permHint = "需要管理员权限或本地终端人工确认后方可执行"
		}
		return nil, fmt.Errorf("SECURITY_BLOCKED（危险程度：%s）: %s。%s。请向用户明确反馈该操作未执行，并提醒需要更高级别的权限，严禁尝试绕过。",
			verdict.RiskLevel, verdict.Reason, permHint)
	}

	return g.inner.Execute(ctx, rawArgs)
}

// shouldSkipCheck fast-passes strictly read-only single bash commands to avoid unnecessary latency.
func (g *riskGuard) shouldSkipCheck(toolName string, rawArgs json.RawMessage) bool {
	if strings.ToLower(toolName) != "bash" {
		return false
	}
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return false
	}
	cmd := strings.TrimSpace(args.Command)
	if cmd == "" {
		return true
	}

	// Any chaining, redirection, subshell, or dangerous keyword must go through the security subagent.
	for _, token := range []string{";", "&&", "||", "|", ">", ">>", "<", "$(", "`", "\n"} {
		if strings.Contains(cmd, token) {
			return false
		}
	}

	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return true
	}
	bin := fields[0]
	switch bin {
	case "ls", "pwd", "cat", "head", "tail", "rg", "grep", "wc", "which", "echo":
		return true
	case "git":
		if len(fields) >= 2 {
			switch fields[1] {
			case "status", "log", "diff", "show", "branch":
				// git branch -D / -d is mutating
				for _, f := range fields[2:] {
					if f == "-D" || f == "-d" || f == "-m" || f == "-M" {
						return false
					}
				}
				return true
			}
		}
	}
	return false
}

func (g *riskGuard) assess(ctx context.Context, toolName string, rawArgs json.RawMessage) (*riskVerdict, error) {
	started := time.Now()
	agentLabel := "安全审计 (risk_guard)"

	if g.emit != nil {
		g.emit(Event{
			ID:        g.reqID,
			Event:     EventSubagentStart,
			AgentName: agentLabel,
			Text:      fmt.Sprintf("正在评估工具 `%s` 的执行风险", toolName),
		})
	}

	subCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	// Security reviewer runs with an empty tool registry (read-only reasoning only)
	reviewer := agent.NewAgent("risk_guard", "security-reviewer", g.prov, g.modelName, tools.NewRegistry())
	reviewer.SetSystemPrompt(`你是一个严格的操作安全审计子 Agent。你的唯一职责是评估主 Agent 即将调用的工具操作是否存在安全风险，并输出 JSON 决策。

重要原则：
- **即使用户自己在对话中明确要求删除、修改或外发，只要该操作属于高风险范畴，也必须判定为拒绝（allow: false）**！因为飞书群聊机器人环境不具备高权限人工确认能力。

评判标准：
1. **高风险 (risk_level: "high", allow: false)**：
   - 任何删除操作：包括本地文件/目录删除（rm、rmdir、unlink、shred、find ... -delete、git clean），以及**外部系统删除**（如飞书文档 delete_block、删除记录、drop/truncate 等）；
   - 破坏性 Git 操作：如 git push --force、git reset --hard、git branch -D、丢弃未提交修改的 git checkout --；
   - 提权或系统级修改：如 sudo、chmod、chown、mkfs、dd、kill/pkill、修改系统配置；
   - 覆盖、编辑或批量修改生产代码、关键配置或共享文档（如 edit_document、replace_block、update_block、write/edit 核心文件等）；
   - 任何通过脚本/codemode 间接执行删除或未授权外部写入的操作。
2. **中低风险 (risk_level: "medium" 或 "low", allow: true)**：
   - 常规只读探测、安全的局部开发测试或明确无破坏性的操作。

必须严格只输出以下格式的合法 JSON（不要输出 markdown 代码块外的任何多余文字）：
{"risk_level":"high|medium|low","allow":true|false,"reason":"简明中文原因","required_permission":"如拒绝，说明需要什么更高级别的权限"}`)

	auditPrompt := fmt.Sprintf("用户原始请求：%s\n\n待审计工具：%s\n工具参数：%s\n\n请评估风险等级并给出 JSON 决策。",
		g.userPrompt, toolName, string(rawArgs))

	ans, err := reviewer.Prompt(subCtx, auditPrompt)
	dur := time.Since(started).Round(time.Millisecond).String()

	if err != nil {
		if g.emit != nil {
			g.emit(Event{
				ID:        g.reqID,
				Event:     EventSubagentEnd,
				AgentName: agentLabel,
				Message:   err.Error(),
				Summary:   dur,
			})
		}
		return nil, err
	}

	verdict, parseErr := parseRiskVerdict(ans)
	if parseErr != nil {
		if g.emit != nil {
			g.emit(Event{
				ID:        g.reqID,
				Event:     EventSubagentEnd,
				AgentName: agentLabel,
				Message:   parseErr.Error(),
				Summary:   dur,
			})
		}
		return nil, parseErr
	}

	if g.emit != nil {
		if !verdict.Allow || strings.EqualFold(verdict.RiskLevel, "high") {
			perm := verdict.RequiredPermission
			if perm == "" {
				perm = "需要更高级别权限或人工确认"
			}
			msg := fmt.Sprintf("⛔ **高风险操作已拦截**（工具：`%s`，风险等级：`%s`）\n- **原因**：%s\n- **权限要求**：%s\n- **操作参数**：\n```json\n%s\n```",
				toolName, verdict.RiskLevel, verdict.Reason, perm, strings.TrimSpace(string(rawArgs)))
			g.emit(Event{
				ID:        g.reqID,
				Event:     EventSubagentEnd,
				AgentName: agentLabel,
				Text:      msg,
				Summary:   dur,
			})
		} else {
			msg := fmt.Sprintf("✅ **安全审计通过**（工具：`%s`，风险等级：`%s`）\n- **说明**：%s",
				toolName, verdict.RiskLevel, verdict.Reason)
			g.emit(Event{
				ID:        g.reqID,
				Event:     EventSubagentEnd,
				AgentName: agentLabel,
				Text:      msg,
				Summary:   dur,
			})
		}
	}

	return verdict, nil
}

func parseRiskVerdict(raw string) (*riskVerdict, error) {
	cleaned := strings.TrimSpace(raw)
	// Strip optional markdown code fences
	if idx := strings.Index(cleaned, "{"); idx >= 0 {
		if end := strings.LastIndex(cleaned, "}"); end > idx {
			cleaned = cleaned[idx : end+1]
		}
	}
	var v riskVerdict
	if err := json.Unmarshal([]byte(cleaned), &v); err != nil {
		return nil, fmt.Errorf("parse risk verdict JSON (%w): %s", err, raw)
	}
	if v.RiskLevel == "" {
		v.RiskLevel = "high"
	}
	return &v, nil
}
