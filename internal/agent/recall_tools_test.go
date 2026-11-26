package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

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

	// Append 20 tool-calling turns (recentCutoff = 15 < 16 -> stays in Epoch 0).
	appendToolTurn := func(idx int) {
		if idx%5 == 0 {
			st.AppendMessage(ai.NewUserMessage(fmt.Sprintf("user prompt %d", idx)))
		}
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

	for i := 0; i < 19; i++ {
		appendToolTurn(i)
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

	// Turn 19 (20th tool turn, cutoff = 15 < 16) stays in Epoch 0 -> 100% byte-identical prefix with asm1.
	appendToolTurn(19)
	asmTurn19 := ag.assembleModelMessages()
	for i := 0; i < len(asm1); i++ {
		if asm1[i].Role != asmTurn19[i].Role || asm1[i].Content != asmTurn19[i].Content {
			t.Fatalf("Epoch 0 prefix mutated at index %d on tool turn 19", i)
		}
	}

	// Turn 20 (21st tool turn, cutoff = 16 -> Epoch 1) folds tool turns 0..15 while keeping turns 16..20 raw.
	appendToolTurn(20)
	asmEpoch1Start := ag.assembleModelMessages()
	var call00Found bool
	for _, m := range asmEpoch1Start {
		if m.ToolCallID == "call_00" {
			call00Found = true
			if !strings.Contains(m.Content, "[已折叠历史工具输出: tool=read call_id=call_00") {
				t.Fatalf("expected call_00 to be masked on Epoch 1 boundary, got %q", m.Content)
			}
		}
		if m.ToolCallID == "call_16" && strings.Contains(m.Content, "[已折叠历史工具输出:") {
			t.Fatalf("expected call_16 to remain raw inside active window, got %q", m.Content)
		}
	}
	if !call00Found {
		t.Fatalf("expected call_00 in assembled messages")
	}

	// Turns 21..35 stay strictly inside Epoch 1 (cutoff = 17..31 < 32):
	// 100% of prior prefix stays byte-identical on every single step!
	prev := marshalWireMessageBytes(t, asmEpoch1Start)
	for extra := 21; extra <= 35; extra++ {
		appendToolTurn(extra)
		asmNext := ag.assembleModelMessages()
		nextBytes := marshalWireMessageBytes(t, asmNext)
		if len(nextBytes) <= len(prev) {
			t.Fatalf("expected append-only growth, got len %d <= %d", len(nextBytes), len(prev))
		}
		for i := 0; i < len(prev); i++ {
			if string(prev[i]) != string(nextBytes[i]) {
				t.Fatalf("Epoch 1 prefix mutated at index %d on tool turn %d!\nwant: %s\ngot:  %s", i, extra, prev[i], nextBytes[i])
			}
		}
		prev = nextBytes
	}
}

func marshalWireMessageBytes(t *testing.T, msgs []ai.Message) [][]byte {
	t.Helper()
	out := make([][]byte, len(msgs))
	for i, m := range msgs {
		// Provider wire serializers ignore local metadata timestamps, which NewSystemMessage refreshes per call.
		m.Timestamp = time.Time{}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("json.Marshal(msg[%d]) error = %v", i, err)
		}
		out[i] = b
	}
	return out
}

func TestEpoch16ToolTurns_ThreeEpochs_Epoch0BytesImmutable(t *testing.T) {
	store, err := sqlite.Open(t.TempDir() + "/three_epochs.db")
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ag := NewAgent("three-epoch-lcp", "sys prompt", nil, "claude-4-sonnet", tools.NewRegistry())
	ag.SetStorage(store)
	st, err := session.NewSessionTree("three-epoch-lcp", store)
	if err != nil {
		t.Fatalf("NewSessionTree() error = %v", err)
	}
	ag.SetSessionTree(st)

	ledger := compaction.NewContextLedger()
	ledger.Apply("add", "cst_go", "runtime", "Go 1.24 + SQLite WAL", "", []string{"e1"}, 1)
	ag.SetContextLedger(ledger)

	largeOutput := strings.Repeat("detailed stacktrace and log payload line\n", 30)
	appendToolTurn := func(turn int) {
		if turn%6 == 0 {
			st.AppendMessage(ai.NewUserMessage(fmt.Sprintf("user guidance at tool turn %d", turn)))
		}
		cid := fmt.Sprintf("call_3ep_%03d", turn)
		st.AppendMessage(ai.Message{
			Role: ai.RoleAssistant,
			ToolCalls: []ai.ToolCall{{
				ID:       cid,
				Type:     "function",
				Function: ai.FunctionCall{Name: "bash", Arguments: json.RawMessage(fmt.Sprintf(`{"step":%d}`, turn))},
			}},
		})
		st.AppendMessage(ai.Message{
			Role:       ai.RoleTool,
			ToolCallID: cid,
			Content:    fmt.Sprintf("output_%03d\n%s", turn, largeOutput),
		})
	}

	// Build through turn 20 -> enters Epoch #1 (tool turns 0..15 frozen; 16..20 recent window).
	for i := 0; i <= 20; i++ {
		appendToolTurn(i)
	}
	asmEp1, metaEp1 := ag.assembleModelMessagesWithMetrics()
	if metaEp1.epochID != 1 {
		t.Fatalf("expected epochID=1 after 21 tool turns, got %d", metaEp1.epochID)
	}

	// Locate the exact message prefix corresponding to Epoch #0 (ending right after call_3ep_015's RoleTool response).
	epoch0MsgLen := 0
	for i, m := range asmEp1 {
		if m.Role == ai.RoleTool && m.ToolCallID == "call_3ep_015" {
			epoch0MsgLen = i + 1
			break
		}
	}
	if epoch0MsgLen == 0 {
		t.Fatalf("failed to find end of Epoch #0 (call_3ep_015) in assembled messages")
	}
	epoch0FrozenBytes := marshalWireMessageBytes(t, asmEp1[:epoch0MsgLen])

	// Advance through Epoch #1 -> Epoch #2 (at turn 36 = 37 tool turns) -> Epoch #3 (at turn 52 = 53 tool turns).
	var epoch1FrozenBytes [][]byte
	epoch1MsgLen := 0
	for turn := 21; turn <= 53; turn++ {
		appendToolTurn(turn)
		asmCurr, metaCurr := ag.assembleModelMessagesWithMetrics()
		currBytes := marshalWireMessageBytes(t, asmCurr)

		// Invariant 1: Every serialized message in Epoch #0 (plus System & ContextLedger) is 100% byte-identical across #1 -> #2 -> #3!
		for i := 0; i < epoch0MsgLen; i++ {
			if string(epoch0FrozenBytes[i]) != string(currBytes[i]) {
				t.Fatalf("Epoch #0 byte mutation at msg[%d] on tool turn %d (epochID=%d)!\nwant: %s\ngot:  %s",
					i, turn, metaCurr.epochID, epoch0FrozenBytes[i], currBytes[i])
			}
		}

		// Record Epoch #1 frozen prefix right when entering Epoch #2 (turn 36 -> epochID == 2), then assert it never mutates across #2 -> #3.
		if turn == 36 {
			if metaCurr.epochID != 2 {
				t.Fatalf("expected epochID=2 at tool turn 36 (37 turns), got %d", metaCurr.epochID)
			}
			for i, m := range asmCurr {
				if m.Role == ai.RoleTool && m.ToolCallID == "call_3ep_031" {
					epoch1MsgLen = i + 1
					break
				}
			}
			if epoch1MsgLen <= epoch0MsgLen {
				t.Fatalf("expected epoch1MsgLen > epoch0MsgLen, got %d <= %d", epoch1MsgLen, epoch0MsgLen)
			}
			epoch1FrozenBytes = currBytes[:epoch1MsgLen]
		} else if turn > 36 {
			for i := 0; i < epoch1MsgLen; i++ {
				if string(epoch1FrozenBytes[i]) != string(currBytes[i]) {
					t.Fatalf("Epoch #1 byte mutation at msg[%d] on tool turn %d (epochID=%d)!", i, turn, metaCurr.epochID)
				}
			}
		}

		if turn == 52 && metaCurr.epochID != 3 {
			t.Fatalf("expected epochID=3 at tool turn 52 (53 turns), got %d", metaCurr.epochID)
		}
	}
}
