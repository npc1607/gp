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

type childRunConfig struct {
	childID    string
	role       string
	taskPrompt string
	cwd        string
	modelName  string
	prov       ai.Provider
	reg        *tools.Registry
	session    *session.SessionTree
	hooks      Hooks
}

// runObservedChild 创建并执行带可观测 Hook 的子 Agent 实例
func runObservedChild(ctx context.Context, cfg childRunConfig) (string, error) {
	child := agent.NewAgent(cfg.childID, cfg.role, cfg.prov, cfg.modelName, cfg.reg)
	child.SetSubagent(true)
	child.SetCwd(cfg.cwd)
	if cfg.session != nil {
		child.SetSessionTree(cfg.session)
	}
	child.SetSystemPrompt(resolveRolePrompt(cfg.role))

	if cfg.hooks.OnStream != nil {
		child.SetStreamHandler(func(chunk ai.StreamChunk) {
			cfg.hooks.OnStream(cfg.childID, chunk)
		})
	}
	if cfg.hooks.OnAgentEvent != nil {
		child.SetEventHandler(func(ev agent.AgentEvent) {
			ev.AgentID = cfg.childID
			cfg.hooks.OnAgentEvent(ev)
		})
	}

	start := time.Now()
	if cfg.hooks.OnLifecycle != nil {
		cfg.hooks.OnLifecycle(Event{
			Type:       "start",
			ChildID:    cfg.childID,
			Role:       cfg.role,
			TaskPrompt: cfg.taskPrompt,
		})
	}

	out, err := child.Prompt(ctx, cfg.taskPrompt)
	dur := time.Since(start)
	if err == nil && strings.TrimSpace(out) == "" {
		err = fmt.Errorf("agent %q 返回空内容", cfg.role)
	}

	if cfg.hooks.OnLifecycle != nil {
		cfg.hooks.OnLifecycle(Event{
			Type:       "end",
			ChildID:    cfg.childID,
			Role:       cfg.role,
			TaskPrompt: cfg.taskPrompt,
			Output:     out,
			Err:        err,
			Duration:   dur,
		})
	}
	if err != nil {
		return "", err
	}
	return out, nil
}

// wireAskPeerHandler 为 ask_peer_agent 工具注入拓扑 B（A2A 信箱 / 按需 Peer 子 Agent）处理器
func wireAskPeerHandler(
	reg *tools.Registry,
	prov ai.Provider,
	modelName, cwd string,
	hub *scheduler.CoordHub,
	ids *idAllocator,
	hooks Hooks,
) {
	rawPeer, ok := reg.Get("ask_peer_agent")
	if !ok {
		return
	}
	askPeer, ok := rawPeer.(*builtin.AskPeerTool)
	if !ok {
		return
	}

	askPeer.SetHandler(func(ctx context.Context, peerRole, question string) (string, error) {
		if hub.Mailbox.HasPeer(peerRole) {
			ans, err := hub.Mailbox.Send(ctx, "peer_caller", peerRole, question)
			slog.Debug("ask_peer", "component", "PEER", "mode", "mailbox", "role", peerRole, "err", err)
			hub.EventBus.Publish(scheduler.BusEvent{
				Topic:   "peer.queried",
				Source:  peerRole,
				Payload: map[string]string{"mode": "mailbox", "question": question},
			})
			return ans, err
		}

		peerID := ids.allocate("sub_peer_" + peerRole)
		defer ids.release(peerID)

		var allowed []string
		for _, tName := range builtin.ResolveToolsForRole(peerRole) {
			if tName != "ask_peer_agent" {
				allowed = append(allowed, tName)
			}
		}
		ans, err := runObservedChild(ctx, childRunConfig{
			childID:    peerID,
			role:       peerRole,
			taskPrompt: question,
			cwd:        cwd,
			modelName:  modelName,
			prov:       prov,
			reg:        reg.ScopedRegistry(allowed),
			hooks:      hooks,
		})
		if err == nil {
			hub.Blackboard.Set("peer:"+peerID, ans)
		}
		slog.Debug("ask_peer", "component", "PEER", "mode", "ephemeral", "id", peerID, "role", peerRole, "err", err)
		hub.EventBus.Publish(scheduler.BusEvent{
			Topic:   "peer.queried",
			Source:  peerID,
			Payload: map[string]string{"mode": "ephemeral_agent", "role": peerRole, "question": question},
		})
		return ans, err
	})
}
