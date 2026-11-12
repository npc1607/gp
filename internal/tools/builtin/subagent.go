package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"gp/internal/core/session"
	"gp/internal/scheduler"
	"gp/internal/tools"
)

// SubagentRunner 子 Agent 协程执行回调函数
type SubagentRunner func(ctx context.Context, role, taskPrompt string, st *session.SessionTree, reg *tools.Registry) (string, error)

// SubagentTask 单个子任务定义
type SubagentTask struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

// SubagentInput subagent 工具输入参数
type SubagentInput struct {
	Tasks []SubagentTask `json:"tasks"`
}

// SubagentTool 动态派发并发子 Agent 的元工具
type SubagentTool struct {
	governor     *scheduler.Governor
	baseRegistry *tools.Registry
	runner       SubagentRunner
	mu           sync.RWMutex
}

// NewSubagentTool 创建 Subagent 工具实例
func NewSubagentTool(gov *scheduler.Governor, baseReg *tools.Registry, runner SubagentRunner) *SubagentTool {
	if gov == nil {
		gov = scheduler.NewGovernor(10, 100000)
	}
	if runner == nil {
		runner = func(ctx context.Context, role, taskPrompt string, st *session.SessionTree, reg *tools.Registry) (string, error) {
			return fmt.Sprintf("[%s] Completed task: %s", role, taskPrompt), nil
		}
	}
	return &SubagentTool{
		governor:     gov,
		baseRegistry: baseReg,
		runner:       runner,
	}
}

func (t *SubagentTool) Name() string {
	return "subagent"
}

func (t *SubagentTool) Description() string {
	return "Concurrently dispatch specialized subagents (scout, reviewer, planner, implementer, verifier) to execute subtasks."
}

func (t *SubagentTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"tasks": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"agent": {
							"type": "string",
							"description": "Role of the subagent (e.g. scout, reviewer, planner, implementer, verifier)"
						},
						"task": {
							"type": "string",
							"description": "The specific task description or instruction"
						}
					},
					"required": ["agent", "task"]
				},
				"description": "List of subagent tasks to execute concurrently"
			}
		},
		"required": ["tasks"]
	}`)
}

// ReplayMode 声明重放策略为 interrupt (派发子 Agent 属于副作用操作)
func (t *SubagentTool) ReplayMode() tools.ReplayMode {
	return tools.ReplayInterrupt
}

// SetRunner 动态注入子 Agent 执行器
func (t *SubagentTool) SetRunner(runner SubagentRunner) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.runner = runner
}

// SetBaseRegistry 动态更新基础工具注册中心
func (t *SubagentTool) SetBaseRegistry(reg *tools.Registry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.baseRegistry = reg
}

// ResolveToolsForRole 根据角色白名单裁剪最小化工具集
func ResolveToolsForRole(role string) []string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "scout":
		return []string{"read"}
	case "reviewer":
		return []string{"read"}
	case "planner":
		return []string{"read"}
	case "verifier":
		return []string{"read", "bash"}
	case "implementer":
		return []string{"read", "write", "edit", "bash", "ask_peer_agent"}
	default:
		return []string{"read"}
	}
}

// Execute 并发派生 Goroutine 执行子任务
func (t *SubagentTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	var input SubagentInput
	if err := json.Unmarshal(rawArgs, &input); err != nil {
		return nil, fmt.Errorf("invalid subagent args: %w", err)
	}

	if len(input.Tasks) == 0 {
		return nil, fmt.Errorf("no tasks provided in subagent call")
	}

	reqCtx := context.Background()
	if ctx != nil && ctx.Context != nil {
		reqCtx = ctx.Context
	}

	t.mu.RLock()
	baseReg := t.baseRegistry
	runner := t.runner
	gov := t.governor
	t.mu.RUnlock()

	results := make([]string, len(input.Tasks))
	var wg sync.WaitGroup

	for i, tsk := range input.Tasks {
		wg.Add(1)
		go func(idx int, task SubagentTask) {
			defer wg.Done()

			childID := fmt.Sprintf("subagent_%s_%d_%d", task.Agent, idx, time.Now().UnixNano())
			st := session.NewInMemorySession(childID)

			var scopedReg *tools.Registry
			if baseReg != nil {
				scopedReg = baseReg.ScopedRegistry(ResolveToolsForRole(task.Agent))
			} else {
				scopedReg = tools.NewRegistry()
			}

			var out string
			var runErr error
			execErr := gov.ExecuteSubagent(reqCtx, func(subCtx context.Context) error {
				var e error
				out, e = runner(subCtx, task.Agent, task.Task, st, scopedReg)
				return e
			})
			if execErr != nil {
				runErr = execErr
			}

			if runErr != nil {
				results[idx] = fmt.Sprintf("[子 Agent: %s] 执行失败: %v", task.Agent, runErr)
			} else {
				results[idx] = fmt.Sprintf("[子 Agent: %s]\n%s", task.Agent, out)
			}
		}(i, tsk)
	}

	wg.Wait()

	var sb strings.Builder
	sb.WriteString("=== 子 Agent 并发执行结果 ===\n")
	for i, res := range results {
		sb.WriteString(res)
		if i < len(results)-1 {
			sb.WriteString("\n\n")
		}
	}

	return &tools.ToolResult{
		Content: sb.String(),
		IsError: false,
	}, nil
}
