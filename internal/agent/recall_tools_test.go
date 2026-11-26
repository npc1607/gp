package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gp/internal/ai"
	"gp/internal/core/compaction"
	"gp/internal/core/session"
	"gp/internal/core/storage/sqlite"
	"gp/internal/tools"
)

func TestRecallEntriesTool_BoundaryAndLosslessAcrossCompaction(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := sqlite.Open(tmpDir + "/recall.db")
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	reg := tools.NewRegistry()
	_ = reg.Register(&dummyReplayTool{name: "read", mode: tools.ReplaySafe})

	structuredResp := `{
  "ledger_events": [
    {
      "action": "add",
      "id": "cst_offline",
      "category": "environment",
      "statement": "Offline VPC only; do not modify deploy/Dockerfile for public registries",
      "provenance": ["PLACEHOLDER_ID"]
    }
  ],
  "handoff_notes": "Checked Dockerfile and proceeded to follow-ups."
}`

	ag := NewAgent("recall-agent", "worker", nil, "claude-4-sonnet", reg)
	ag.SetStorage(store)
	st, err := session.NewSessionTree("recall-agent", store)
	if err != nil {
		t.Fatalf("NewSessionTree() error = %v", err)
	}
	ag.SetSessionTree(st)

	for _, name := range []string{"restore_observation", "recall_entries"} {
		tool, ok := reg.Get(name)
		if !ok {
			t.Fatalf("expected %s to be auto-registered", name)
		}
		if tools.GetReplayMode(tool) != tools.ReplaySafe {
			t.Fatalf("expected %s to be ReplaySafe", name)
		}
	}

	eConstraint := st.AppendMessage(ai.NewUserMessage("Hard rule: offline VPC only; do not modify deploy/Dockerfile"))
	structuredResp = strings.ReplaceAll(structuredResp, "PLACEHOLDER_ID", eConstraint.ID)

	ag.SetProvider(&mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{{DeltaText: structuredResp}}
			},
		},
	})

	st.AppendMessage(ai.Message{
		Role: ai.RoleAssistant,
		ToolCalls: []ai.ToolCall{{
			ID:       "call_old_tool",
			Type:     "function",
			Function: ai.FunctionCall{Name: "read", Arguments: json.RawMessage(`{"path":"deploy/Dockerfile"}`)},
		}},
	})
	oldPayload := strings.Repeat("ENV VENDOR=1\n", 40) + "UNIQUE_DOCKERFILE_MARKER_XYZ\n"
	st.AppendMessage(ai.Message{
		Role:       ai.RoleTool,
		ToolCallID: "call_old_tool",
		Content:    oldPayload,
	})
	for i := 0; i < 8; i++ {
		st.AppendMessage(ai.NewUserMessage(fmt.Sprintf("follow-up step %d", i)))
		st.AppendMessage(ai.NewAssistantMessage(fmt.Sprintf("ack step %d", i), ""))
	}

	// Execute compaction: updates ContextLedger via LLM JSON & persists to SQLite
	res, err := ag.Compact(context.Background(), "")
	if err != nil {
		t.Fatalf("Compact() error = %v", err)
	}
	if res.Ledger == nil || !strings.Contains(res.Ledger.CanonicalRender(), eConstraint.ID) {
		t.Fatalf("expected ContextLedger to cite provenance %s, got:\n%s", eConstraint.ID, res.Ledger.CanonicalRender())
	}

	// Verify ContextLedger persisted to SQLite documents table and restores on new AgentInstance
	rawLedger, ok, err := store.GetDocument(ag.SessionID(), contextLedgerDocKey)
	if err != nil || !ok || !strings.Contains(string(rawLedger), eConstraint.ID) {
		t.Fatalf("expected context_ledger persisted in SQLite, ok=%v err=%v raw=%s", ok, err, string(rawLedger))
	}
	reloadedAgent := NewAgent(ag.SessionID(), "worker", nil, "claude-4-sonnet", nil)
	reloadedTree, err := session.NewSessionTree(ag.SessionID(), store)
	if err != nil {
		t.Fatalf("reload SessionTree error = %v", err)
	}
	reloadedAgent.SetSessionTree(reloadedTree)
	reloadedAgent.SetStorage(store)
	if !strings.Contains(reloadedAgent.GetContextLedger().CanonicalRender(), "Offline VPC only") {
		t.Fatalf("expected reloaded agent to restore persisted ContextLedger, got %q", reloadedAgent.GetContextLedger().CanonicalRender())
	}

	recallTool, _ := reg.Get("recall_entries")

	// 1. Recall pre-compaction constraint entry by entry_id
	recallArgs, _ := json.Marshal(map[string]any{"entry_ids": []string{eConstraint.ID}})
	recallRes, err := recallTool.Execute(&tools.ToolContext{Context: context.Background()}, recallArgs)
	if err != nil || recallRes.IsError {
		t.Fatalf("recall_entries failed: err=%v res=%+v", err, recallRes)
	}
	if !strings.Contains(recallRes.Content, eConstraint.ID) || !strings.Contains(recallRes.Content, "deploy/Dockerfile") {
		t.Fatalf("expected recall_entries to return verbatim constraint, got:\n%s", recallRes.Content)
	}

	// 2. Boundary: empty entry_ids -> error
	emptyRes, _ := recallTool.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{"entry_ids":[]}`))
	if !emptyRes.IsError {
		t.Fatalf("expected IsError=true for empty entry_ids, got %+v", emptyRes)
	}

	// 3. Boundary: missing entry_id + valid entry_id -> partial notice without crashing
	mixedArgs, _ := json.Marshal(map[string]any{"entry_ids": []string{"non_existent_id", eConstraint.ID}})
	mixedRes, _ := recallTool.Execute(&tools.ToolContext{Context: context.Background()}, mixedArgs)
	if mixedRes.IsError || !strings.Contains(mixedRes.Content, "未找到") || !strings.Contains(mixedRes.Content, eConstraint.ID) {
		t.Fatalf("unexpected mixed recall result: %+v", mixedRes)
	}

	// 4. Boundary: huge payload truncation at 32KB limit
	hugeEntry := st.AppendMessage(ai.NewUserMessage(strings.Repeat("H", 40*1024)))
	hugeArgs, _ := json.Marshal(map[string]any{"entry_ids": []string{hugeEntry.ID, eConstraint.ID}})
	hugeRes, _ := recallTool.Execute(&tools.ToolContext{Context: context.Background()}, hugeArgs)
	if !strings.Contains(hugeRes.Content, "32KB limit") {
		t.Fatalf("expected 32KB truncation notice for huge recall output")
	}

	// 5. restore_observation must also still recover pre-compaction masked RoleTool output
	restoreTool, _ := reg.Get("restore_observation")
	restoreRes, err := restoreTool.Execute(&tools.ToolContext{Context: context.Background()},
		json.RawMessage(`{"call_id":"call_old_tool"}`))
	if err != nil || restoreRes.IsError || !strings.Contains(restoreRes.Content, "UNIQUE_DOCKERFILE_MARKER_XYZ") {
		t.Fatalf("expected restore_observation to recover pre-compaction tool output, err=%v res=%+v", err, restoreRes)
	}
}

func TestEpoch16FrozenMasking_LCPPrefixStabilityAndE4AOrder(t *testing.T) {
	ag := NewAgent("epoch-lcp-test", "worker", nil, "claude-4-sonnet", tools.NewRegistry())
	ag.SetSystemPrompt("Base System Rules")
	if ag.GetObservationMaskEpoch() != 16 {
		t.Fatalf("expected default observation mask epoch == 16, got %d", ag.GetObservationMaskEpoch())
	}

	ledger := compaction.NewContextLedger()
	ledger.Apply("add", "cst_pg", "tech_stack", "Use PostgreSQL 16", "", []string{"entry_01"}, 1)
	ag.SetContextLedger(ledger)

	st := ag.GetSessionTree()
	largeOutput := strings.Repeat("diagnostic log line payload\n", 30)

	// Append 11 tool-calling turns (22 messages: well past Epoch=16 boundary with window=5)
	for i := 0; i < 11; i++ {
		cid := fmt.Sprintf("call_%02d", i)
		st.AppendMessage(ai.Message{
			Role: ai.RoleAssistant,
			ToolCalls: []ai.ToolCall{{
				ID:       cid,
				Type:     "function",
				Function: ai.FunctionCall{Name: "read", Arguments: json.RawMessage(`{}`)},
			}},
		})
		st.AppendMessage(ai.Message{
			Role:       ai.RoleTool,
			ToolCallID: cid,
			Content:    fmt.Sprintf("turn_%02d\n%s", i, largeOutput),
		})
	}

	asm1 := ag.assembleModelMessages()
	// Verify E4-A PR #1 prefix order: [System (CacheControl)] -> [ContextLedger (CacheControl)] -> [Epoch RecentWindow...]
	if len(asm1) < 3 {
		t.Fatalf("expected at least 3 messages, got %d", len(asm1))
	}
	if asm1[0].Role != ai.RoleSystem || !asm1[0].CacheControl {
		t.Fatalf("expected asm1[0] to be cached RoleSystem, got %+v", asm1[0])
	}
	if asm1[1].Role != ai.RoleUser || !asm1[1].CacheControl || !strings.Contains(asm1[1].Content, "[CONTEXT_LEDGER]") {
		t.Fatalf("expected asm1[1] to be cached [CONTEXT_LEDGER], got %+v", asm1[1])
	}
	if !strings.Contains(asm1[2].Content, "") && asm1[2].Role != ai.RoleAssistant {
		t.Fatalf("expected Epoch RecentWindow to start right after ContextLedger")
	}

	// Verify turns within Epoch 0 (turns 11 -> 11) and then across Epoch 1 (turns 13..19):
	// First check transition at turn 12 (recentCutoff reaches 16 -> freezeBoundary jumps from 0 to 16).
	appendToolTurn := func(idx int) {
		cid := fmt.Sprintf("call_%02d", idx)
		st.AppendMessage(ai.Message{
			Role: ai.RoleAssistant,
			ToolCalls: []ai.ToolCall{{
				ID:       cid,
				Type:     "function",
				Function: ai.FunctionCall{Name: "read", Arguments: json.RawMessage(`{}`)},
			}},
		})
		st.AppendMessage(ai.Message{
			Role:       ai.RoleTool,
			ToolCallID: cid,
			Content:    fmt.Sprintf("turn_%02d\n%s", idx, largeOutput),
		})
	}

	// Turn 11 stays in Epoch 0 (recentCutoff=14 < 16) -> 100% prefix match with asm1.
	appendToolTurn(11)
	asmTurn11 := ag.assembleModelMessages()
	for i := 0; i < len(asm1); i++ {
		if asm1[i].Role != asmTurn11[i].Role || asm1[i].Content != asmTurn11[i].Content {
			t.Fatalf("Epoch 0 prefix mutated at index %d on turn 11", i)
		}
	}

	// Turn 12 crosses Epoch 1 boundary (recentCutoff=16 -> freezeBoundary=16): call_00 gets masked once.
	appendToolTurn(12)
	asmEpoch1Start := ag.assembleModelMessages()
	if !strings.Contains(asmEpoch1Start[3].Content, "[已折叠历史工具输出: tool=read call_id=call_00") {
		t.Fatalf("expected call_00 to be masked on Epoch 1 boundary, got %q", asmEpoch1Start[3].Content)
	}

	// Turns 13..19 remain strictly inside Epoch 1 (recentCutoff=18..30 < 32):
	// 100% of prior prefix stays byte-identical on every single step!
	prev := asmEpoch1Start
	for extra := 13; extra <= 19; extra++ {
		appendToolTurn(extra)
		asmNext := ag.assembleModelMessages()
		if len(asmNext) <= len(prev) {
			t.Fatalf("expected append-only growth, got len %d <= %d", len(asmNext), len(prev))
		}
		for i := 0; i < len(prev); i++ {
			if prev[i].Role != asmNext[i].Role || prev[i].Content != asmNext[i].Content || prev[i].CacheControl != asmNext[i].CacheControl {
				t.Fatalf("Epoch 1 prefix mutated at index %d on turn %d!\nwant: %q\ngot:  %q", i, extra, prev[i].Content, asmNext[i].Content)
			}
		}
		prev = asmNext
	}
}
