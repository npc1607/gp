package subagent

import (
	"fmt"
	"strings"
	"sync"
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

type idAllocator struct {
	mu     sync.Mutex
	active map[string]bool
}

func newIDAllocator() *idAllocator {
	return &idAllocator{active: make(map[string]bool)}
}

func (a *idAllocator) allocate(prefix string) string {
	base := sanitizeID(prefix)
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.active[base] {
		a.active[base] = true
		return base
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s_%d", base, i)
		if !a.active[cand] {
			a.active[cand] = true
			return cand
		}
	}
}

func (a *idAllocator) release(id string) {
	a.mu.Lock()
	delete(a.active, id)
	a.mu.Unlock()
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
