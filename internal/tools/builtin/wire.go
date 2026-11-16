package builtin

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"gp/internal/agent"
	"gp/internal/ai"
	"gp/internal/core/session"
	"gp/internal/tools"
)

// roleSystemPrompts 各工种子 Agent 的角色系统提示词
var roleSystemPrompts = map[string]string{
	"scout":       "你是侦察兵 (scout)。你的职责是快速探查代码库与外部环境，只做只读调研，输出精确的事实发现：文件路径、行号、关键符号、依赖关系。不要臆测，不要修改任何文件。",
	"reviewer":    "你是代码评审员 (reviewer)。你的职责是挑出缺陷、安全风险、并发隐患与可维护性问题。输出必须指出具体位置与原因，并按严重程度分级。",
	"planner":     "你是规划师 (planner)。你的职责是把模糊目标拆解为可执行的步骤序列，标明依赖关系与验收标准。只输出计划，不执行实现。",
	"implementer": "你是实现工程师 (implementer)。你的职责是按任务要求落地代码，遵循现有代码风格，改动最小化，完成后简述改了什么、为什么改。",
	"verifier":    "你是验收员 (verifier)。你的职责是用编译、测试、静态检查等手段验证改动是否正确，输出可复现的验证命令与真实结果。不确定就写明不确定。",
}

// defaultRoleSystemPrompt 未匹配到工种时的通用角色提示词
const defaultRoleSystemPrompt = "你是一个独立执行的子 Agent。你只负责完成分配给你的单一任务，输出要具体、可核查，不要寒暄，不要复述任务描述。"

// resolveRolePrompt 根据角色名解析系统提示词
func resolveRolePrompt(role string) string {
	key := strings.ToLower(strings.TrimSpace(role))
	if p, ok := roleSystemPrompts[key]; ok {
		return p
	}
	return defaultRoleSystemPrompt
}

// SubagentEvent 描述子 Agent 生命周期与数据流事件（派发输入与返回输出）
type SubagentEvent struct {
	Type       string // "start" | "end"
	ChildID    string
	Role       string
	TaskPrompt string
	Output     string
	Err        error
	Duration   time.Duration
}

// SubagentHooks 允许宿主（如交互式 TUI）观测子 Agent 的实时流、事件与数据流
type SubagentHooks struct {
	OnLifecycle  func(ev SubagentEvent)
	OnStream     func(agentID string, chunk ai.StreamChunk)
	OnAgentEvent func(ev agent.AgentEvent)
}

// WireSubagentRunner 为注册中心里的 subagent 工具注入真实的子 Agent 执行器。
//
// 未接线时 SubagentTool 会退化为直连 stub，只回显 "Completed task: ..." 而不真正调用模型，
// 导致主 Agent 拿到一堆空回执。此处将 runner 指向 agent.RunLoop 驱动的真实子 Agent 协程。
func WireSubagentRunner(reg *tools.Registry, prov ai.Provider, modelName, cwd string) error {
	return WireSubagentRunnerWithHooks(reg, prov, modelName, cwd, SubagentHooks{})
}

// WireSubagentRunnerWithHooks 注入真实子 Agent 执行器，并挂载可观测回调以追踪子 Agent 状态与数据流
func WireSubagentRunnerWithHooks(reg *tools.Registry, prov ai.Provider, modelName, cwd string, hooks SubagentHooks) error {
	if reg == nil {
		return fmt.Errorf("wire subagent runner: registry is nil")
	}
	if prov == nil {
		return fmt.Errorf("wire subagent runner: provider is nil")
	}
	tool, ok := reg.Get("subagent")
	if !ok {
		return fmt.Errorf("wire subagent runner: tool %q not registered", "subagent")
	}
	sub, ok := tool.(*SubagentTool)
	if !ok {
		return fmt.Errorf("wire subagent runner: tool %q has unexpected type %T", "subagent", tool)
	}

	var activeMu sync.Mutex
	activeIDs := make(map[string]bool)

	allocateChildID := func(role string) string {
		base := "sub_" + sanitizeID(role)
		activeMu.Lock()
		defer activeMu.Unlock()
		if !activeIDs[base] {
			activeIDs[base] = true
			return base
		}
		for i := 2; ; i++ {
			cand := fmt.Sprintf("%s_%d", base, i)
			if !activeIDs[cand] {
				activeIDs[cand] = true
				return cand
			}
		}
	}

	releaseChildID := func(id string) {
		activeMu.Lock()
		delete(activeIDs, id)
		activeMu.Unlock()
	}

	sub.SetRunner(func(ctx context.Context, role, taskPrompt string, st *session.SessionTree, scopedReg *tools.Registry) (string, error) {
		childID := allocateChildID(role)
		defer releaseChildID(childID)
		child := agent.NewAgent(childID, role, prov, modelName, scopedReg)
		child.SetCwd(cwd)
		if st != nil {
			child.SetSessionTree(st)
		}
		child.SetSystemPrompt(resolveRolePrompt(role))

		if hooks.OnStream != nil {
			child.SetStreamHandler(func(chunk ai.StreamChunk) {
				hooks.OnStream(childID, chunk)
			})
		}
		if hooks.OnAgentEvent != nil {
			child.SetEventHandler(func(ev agent.AgentEvent) {
				ev.AgentID = childID
				hooks.OnAgentEvent(ev)
			})
		}

		start := time.Now()
		if hooks.OnLifecycle != nil {
			hooks.OnLifecycle(SubagentEvent{
				Type:       "start",
				ChildID:    childID,
				Role:       role,
				TaskPrompt: taskPrompt,
			})
		}

		out, err := child.Prompt(ctx, taskPrompt)
		dur := time.Since(start)
		if err == nil && strings.TrimSpace(out) == "" {
			err = fmt.Errorf("子 Agent %q 返回空内容", role)
		}

		if hooks.OnLifecycle != nil {
			hooks.OnLifecycle(SubagentEvent{
				Type:       "end",
				ChildID:    childID,
				Role:       role,
				TaskPrompt: taskPrompt,
				Output:     out,
				Err:        err,
				Duration:   dur,
			})
		}
		if err != nil {
			return "", err
		}
		return out, nil
	})
	return nil
}

// sanitizeID 将角色名清洗为可用于 Agent ID 的字符序列
func sanitizeID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "agent"
	}
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	return sb.String()
}
