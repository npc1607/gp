package subagent

import (
	"context"
	"encoding/json"
	"testing"

	"gp/internal/ai"
	"gp/internal/scheduler"
	"gp/internal/tools"
	"gp/internal/tools/builtin"
)

type mockStaticProvider struct {
	reply string
}

func (m *mockStaticProvider) Name() string { return "mock" }

func (m *mockStaticProvider) StreamChat(ctx context.Context, req *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	ch := make(chan ai.StreamChunk, 1)
	ch <- ai.StreamChunk{DeltaText: m.reply}
	close(ch)
	return ch, nil
}

func TestWireRunnerWithHub_AllThreeTopologies(t *testing.T) {
	reg, err := builtin.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("NewDefaultRegistry() error = %v", err)
	}
	hub := scheduler.NewCoordHub()

	// 1. Register a live Actor Mailbox for "reviewer"
	unregReviewer := hub.Mailbox.RegisterHandler("reviewer", func(ctx context.Context, msg *scheduler.PeerMessage) (string, error) {
		return "mailbox-reply: " + msg.Payload, nil
	})
	defer unregReviewer()

	prov := &mockStaticProvider{reply: "agent-output-ok"}
	if err := WireRunnerWithHooks(reg, prov, "mock-model", t.TempDir(), Hooks{Hub: hub}); err != nil {
		t.Fatalf("WireRunnerWithHooks() error = %v", err)
	}

	// 2. Test SubagentTool -> writes Blackboard + publishes EventBus
	subTool, _ := reg.Get("subagent")
	rawSub, _ := json.Marshal(map[string]any{
		"tasks": []map[string]string{
			{"agent": "scout", "task": "inspect"},
		},
	})
	res, err := subTool.Execute(&tools.ToolContext{Cwd: t.TempDir()}, rawSub)
	if err != nil {
		t.Fatalf("subagent Execute error = %v", err)
	}
	if res.IsError {
		t.Fatalf("subagent returned error: %s", res.Content)
	}
	if val, ok := hub.Blackboard.Get("role:scout"); !ok || val != "agent-output-ok" {
		t.Fatalf("Blackboard['role:scout'] = (%v, %v), want 'agent-output-ok'", val, ok)
	}
	if hist := hub.EventBus.History("subagent.*", 0); len(hist) < 2 {
		t.Fatalf("expected >= 2 subagent.* events in EventBus, got %d", len(hist))
	}

	// 3. Test AskPeerTool -> routes to live Mailbox when registered
	peerTool, _ := reg.Get("ask_peer_agent")
	rawPeerMailbox, _ := json.Marshal(map[string]string{
		"peer_role": "reviewer",
		"question":  "is this safe?",
	})
	peerRes, err := peerTool.Execute(&tools.ToolContext{Cwd: t.TempDir()}, rawPeerMailbox)
	if err != nil {
		t.Fatalf("ask_peer_agent (mailbox) error = %v", err)
	}
	if peerRes.Content != "[reviewer 的回答]: mailbox-reply: is this safe?" {
		t.Fatalf("ask_peer_agent (mailbox) = %q, want '[reviewer 的回答]: mailbox-reply: is this safe?'", peerRes.Content)
	}

	// 4. Test AskPeerTool -> spawns ephemeral peer agent when no mailbox registered
	rawPeerEphemeral, _ := json.Marshal(map[string]string{
		"peer_role": "planner",
		"question":  "what is step 1?",
	})
	ephRes, err := peerTool.Execute(&tools.ToolContext{Cwd: t.TempDir()}, rawPeerEphemeral)
	if err != nil {
		t.Fatalf("ask_peer_agent (ephemeral) error = %v", err)
	}
	if ephRes.Content != "[planner 的回答]: agent-output-ok" {
		t.Fatalf("ask_peer_agent (ephemeral) = %q, want '[planner 的回答]: agent-output-ok'", ephRes.Content)
	}
	if hist := hub.EventBus.History("peer.queried", 0); len(hist) != 2 {
		t.Fatalf("expected 2 peer.queried events, got %d", len(hist))
	}
}
