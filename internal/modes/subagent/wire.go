package subagent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gp/internal/agent"
	"gp/internal/ai"
	"gp/internal/core/session"
	"gp/internal/scheduler"
	"gp/internal/tools"
	"gp/internal/tools/builtin"
)

// Event 描述子 Agent 生命周期与数据流事件（派发输入与返回输出）
type Event struct {
	Type       string // "start" | "end"
	ChildID    string
	Role       string
	TaskPrompt string
	Output     string
	Err        error
	Duration   time.Duration
}

// Hooks 允许宿主（如交互式 TUI）观测子 Agent 的实时流、事件与数据流
type Hooks struct {
	Hub          *scheduler.CoordHub
	OnLifecycle  func(ev Event)
	OnStream     func(agentID string, chunk ai.StreamChunk)
	OnAgentEvent func(ev agent.AgentEvent)
}

// WireRunner 为注册中心里的 subagent 与 ask_peer_agent 工具注入真实的子 Agent 执行器。
func WireRunner(reg *tools.Registry, prov ai.Provider, modelName, cwd string) error {
	return WireRunnerWithHooks(reg, prov, modelName, cwd, Hooks{})
}

// WireRunnerWithHooks 注入真实子 Agent 执行器，并挂载三大通信拓扑与可观测回调
func WireRunnerWithHooks(reg *tools.Registry, prov ai.Provider, modelName, cwd string, hooks Hooks) error {
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
	sub, ok := tool.(*builtin.SubagentTool)
	if !ok {
		return fmt.Errorf("wire subagent runner: tool %q has unexpected type %T", "subagent", tool)
	}

	hub := hooks.Hub
	if hub == nil {
		hub = scheduler.NewCoordHub()
	}
	ids := newIDAllocator()

	sub.SetRunner(func(ctx context.Context, role, taskPrompt string, st *session.SessionTree, scopedReg *tools.Registry) (string, error) {
		childID := ids.allocate("sub_" + role)
		defer ids.release(childID)

		hub.EventBus.Publish(scheduler.BusEvent{
			Topic:   "subagent.started",
			Source:  childID,
			Payload: map[string]string{"role": role, "task": taskPrompt},
		})

		out, err := runObservedChild(ctx, childRunConfig{
			childID:    childID,
			role:       role,
			taskPrompt: taskPrompt,
			cwd:        cwd,
			modelName:  modelName,
			prov:       prov,
			reg:        scopedReg,
			session:    st,
			hooks:      hooks,
		})
		if err == nil {
			hub.Blackboard.Set(childID, out)
			hub.Blackboard.Set("role:"+strings.ToLower(strings.TrimSpace(role)), out)
			slog.Debug("subagent completed", "component", "SUBAGENT", "id", childID, "role", role, "out_len", len(out))
			hub.EventBus.Publish(scheduler.BusEvent{
				Topic:   "subagent.completed",
				Source:  childID,
				Payload: out,
			})
		} else {
			slog.Debug("subagent failed", "component", "SUBAGENT", "id", childID, "role", role, "err", err)
			hub.EventBus.Publish(scheduler.BusEvent{
				Topic:   "subagent.failed",
				Source:  childID,
				Payload: err.Error(),
			})
		}
		return out, err
	})

	wireAskPeerHandler(reg, prov, modelName, cwd, hub, ids, hooks)
	return nil
}
