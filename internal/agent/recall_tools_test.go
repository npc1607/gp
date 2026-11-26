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
	"gp/internal/core/storage"
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

	ag := NewAgent("three-epoch-lcp", "worker", nil, "claude-4-sonnet", tools.NewRegistry())
	ag.SetSystemPrompt("sys prompt")
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

	// Locate [System, ContextLedger], optional [WorkingSet], and Epoch #0 (ending right after call_3ep_015's RoleTool response).
	findEpochWindowBounds := func(msgs []ai.Message, targetCallID string) (startIdx, endIdx int) {
		startIdx = -1
		for i, m := range msgs {
			if m.Role == ai.RoleSystem || (m.Role == ai.RoleUser && (strings.HasPrefix(m.Content, "[CONTEXT_LEDGER]") || strings.HasPrefix(m.Content, "<working_set>"))) {
				continue
			}
			if startIdx < 0 {
				startIdx = i
			}
			if m.Role == ai.RoleTool && m.ToolCallID == targetCallID {
				return startIdx, i + 1
			}
		}
		return -1, -1
	}

	ep0Start, ep0End := findEpochWindowBounds(asmEp1, "call_3ep_015")
	if ep0Start < 0 || ep0End <= ep0Start {
		t.Fatalf("failed to locate Epoch #0 slice in assembled messages: start=%d end=%d", ep0Start, ep0End)
	}
	headerFrozenBytes := marshalWireMessageBytes(t, asmEp1[:2]) // System + ContextLedger
	epoch0FrozenBytes := marshalWireMessageBytes(t, asmEp1[ep0Start:ep0End])

	// Advance through Epoch #1 -> Epoch #2 (at turn 36 = 37 tool turns) -> Epoch #3 (at turn 52 = 53 tool turns).
	var sameEpochPrev [][]byte
	prevEpoch := metaEp1.epochID
	var epoch1FrozenBytes [][]byte
	for turn := 21; turn <= 53; turn++ {
		appendToolTurn(turn)
		asmCurr, metaCurr := ag.assembleModelMessagesWithMetrics()
		currBytes := marshalWireMessageBytes(t, asmCurr)

		// Invariant 1: System + ContextLedger stay 100% byte-identical across ALL turns (#1 -> #2 -> #3).
		for i := 0; i < len(headerFrozenBytes); i++ {
			if string(headerFrozenBytes[i]) != string(currBytes[i]) {
				t.Fatalf("Header msg[%d] mutated on tool turn %d!", i, turn)
			}
		}

		// Invariant 2: Within the same Epoch, the entire prior prefix (including WorkingSet) stays 100% byte-identical!
		if metaCurr.epochID == prevEpoch && len(sameEpochPrev) > 0 {
			for i := 0; i < len(sameEpochPrev); i++ {
				if string(sameEpochPrev[i]) != string(currBytes[i]) {
					t.Fatalf("Same-Epoch (%d) prefix mutated at msg[%d] on tool turn %d!\nwant: %s\ngot:  %s",
						metaCurr.epochID, i, turn, sameEpochPrev[i], currBytes[i])
				}
			}
		}
		sameEpochPrev = currBytes
		prevEpoch = metaCurr.epochID

		// Invariant 3: Across Epoch #1 -> #2 -> #3 transitions, ALL frozen Epoch #0 messages stay 100% byte-identical!
		currEp0Start, currEp0End := findEpochWindowBounds(asmCurr, "call_3ep_015")
		if currEp0End-currEp0Start != len(epoch0FrozenBytes) {
			t.Fatalf("Epoch #0 message count changed on turn %d: got %d want %d", turn, currEp0End-currEp0Start, len(epoch0FrozenBytes))
		}
		for i := 0; i < len(epoch0FrozenBytes); i++ {
			if string(epoch0FrozenBytes[i]) != string(currBytes[currEp0Start+i]) {
				t.Fatalf("Epoch #0 byte mutation at local idx %d on tool turn %d (epochID=%d)!\nwant: %s\ngot:  %s",
					i, turn, metaCurr.epochID, epoch0FrozenBytes[i], currBytes[currEp0Start+i])
			}
		}

		// Record Epoch #1 frozen prefix right when entering Epoch #2 (turn 36 -> epochID == 2), then assert it never mutates across #2 -> #3.
		if turn == 36 {
			if metaCurr.epochID != 2 {
				t.Fatalf("expected epochID=2 at tool turn 36 (37 turns), got %d", metaCurr.epochID)
			}
			_, ep1End := findEpochWindowBounds(asmCurr, "call_3ep_031")
			ep1Start := currEp0End
			if ep1End <= ep1Start {
				t.Fatalf("expected ep1End > ep1Start, got %d <= %d", ep1End, ep1Start)
			}
			epoch1FrozenBytes = currBytes[ep1Start:ep1End]
		} else if turn > 36 {
			for i := 0; i < len(epoch1FrozenBytes); i++ {
				if string(epoch1FrozenBytes[i]) != string(currBytes[currEp0End+i]) {
					t.Fatalf("Epoch #1 byte mutation at local idx %d on tool turn %d (epochID=%d)!", i, turn, metaCurr.epochID)
				}
			}
		}

		if turn == 52 && metaCurr.epochID != 3 {
			t.Fatalf("expected epochID=3 at tool turn 52 (53 turns), got %d", metaCurr.epochID)
		}
	}
}

func TestWorkingSet_ShortSessionZeroOverhead(t *testing.T) {
	ag := NewAgent("ws-short-session", "sys", nil, "claude-4-sonnet", tools.NewRegistry())
	st := ag.GetSessionTree()
	st.AppendMessage(ai.NewUserMessage("check internal/agent/compact.go"))
	st.AppendMessage(ai.Message{
		Role: ai.RoleAssistant,
		ToolCalls: []ai.ToolCall{{
			ID:       "call_short_0",
			Type:     "function",
			Function: ai.FunctionCall{Name: "read", Arguments: json.RawMessage(`{"file":"compact.go"}`)},
		}},
	})
	st.AppendMessage(ai.Message{
		Role:       ai.RoleTool,
		ToolCallID: "call_short_0",
		Content:    strings.Repeat("short log line\n", 30),
	})

	msgs, meta := ag.assembleModelMessagesWithMetrics()
	if meta.epochMaskedCount != 0 || meta.workingSetTokens != 0 || meta.workingSetEntries != 0 || meta.retrievalBuildMs != 0 {
		t.Fatalf("expected zero WorkingSet overhead on short unmasked session, got %+v", meta)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "<working_set>") {
			t.Fatalf("short session must NOT inject <working_set>, got %q", m.Content)
		}
	}
}

func TestWorkingSet_EpochFrozenZeroJitterAndRefreshesOnTransition(t *testing.T) {
	store, err := sqlite.Open(t.TempDir() + "/ws_frozen.db")
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	prov := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{{DeltaText: `{"ledger_events":[],"handoff_notes":"compacted summary"}`}}
			},
		},
	}

	ag := NewAgent("ws-frozen", "worker", prov, "claude-4-sonnet", tools.NewRegistry())
	ag.SetSystemPrompt("sys rules")
	ag.SetStorage(store)
	st, err := session.NewSessionTree("ws-frozen", store)
	if err != nil {
		t.Fatalf("NewSessionTree() error = %v", err)
	}
	ag.SetSessionTree(st)

	ag.ApplyLedgerEvents(context.Background(), []compaction.LedgerEvent{{
		Action:     "add",
		ID:         "cst_go",
		Category:   "runtime",
		Statement:  "Go 1.24 only",
		Provenance: []string{"seed"},
		Turn:       1,
	}})

	large := strings.Repeat("filler diagnostic payload\n", 25)
	st.AppendMessage(ai.NewUserMessage("inspect proto/analytics/audit_meta.proto and AuditSlotVerifier"))
	for i := 0; i < 21; i++ {
		cid := fmt.Sprintf("call_ws_%02d", i)
		payload := fmt.Sprintf("step=%d\n%s", i, large)
		if i == 2 {
			payload = fmt.Sprintf("step=2 proto/analytics/audit_meta.proto AuditSlotVerifier reserved 12\n%s", large)
		}
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
			Content:    payload,
		})
	}
	st.AppendMessage(ai.NewUserMessage("Explain AuditSlotVerifier in proto/analytics/audit_meta.proto"))

	asm1, meta1 := ag.assembleModelMessagesWithMetrics()
	if meta1.epochID != 1 || meta1.workingSetEntries == 0 || meta1.workingSetTokens <= 0 {
		t.Fatalf("expected Epoch 1 to inject non-empty WorkingSet, got ep=%d entries=%d toks=%d", meta1.epochID, meta1.workingSetEntries, meta1.workingSetTokens)
	}
	if meta1.strategyMode != strategyModeE4APR2 || meta1.graphMatchedEntities == 0 {
		t.Fatalf("expected strategyMode=e4a_pr2 and graph_matched_entities > 0, got %+v", meta1)
	}
	if len(asm1) < 4 || !strings.HasPrefix(asm1[2].Content, "<working_set>") || !asm1[2].CacheControl {
		t.Fatalf("expected E4-A order [System, Ledger, <working_set>(cached), ...], got asm1[0..2]=%+v", asm1[:3])
	}
	if !strings.Contains(asm1[2].Content, "AuditSlotVerifier") {
		t.Fatalf("expected Epoch-masked AuditSlotVerifier evidence recalled into <working_set>, got:\n%s", asm1[2].Content)
	}

	// Within Epoch #1: follow-up user & tool messages must NOT change <working_set> or prefix bytes!
	st.AppendMessage(ai.NewUserMessage("Wait, what about completely unrelated docker/buildx/config.yaml?"))
	st.AppendMessage(ai.Message{
		Role: ai.RoleAssistant,
		ToolCalls: []ai.ToolCall{{
			ID:       "call_ws_21",
			Type:     "function",
			Function: ai.FunctionCall{Name: "read", Arguments: json.RawMessage(`{}`)},
		}},
	})
	st.AppendMessage(ai.Message{
		Role:       ai.RoleTool,
		ToolCallID: "call_ws_21",
		Content:    "docker buildx ok",
	})

	asmSameEp, metaSameEp := ag.assembleModelMessagesWithMetrics()
	if metaSameEp.epochID != 1 || metaSameEp.retrievalBuildMs != 0 {
		t.Fatalf("expected Epoch 1 cache hit (retrievalBuildMs=0), got ep=%d buildMs=%.3f", metaSameEp.epochID, metaSameEp.retrievalBuildMs)
	}
	b1 := marshalWireMessageBytes(t, asm1)
	bSame := marshalWireMessageBytes(t, asmSameEp)
	for i := 0; i < len(b1); i++ {
		if string(b1[i]) != string(bSame[i]) {
			t.Fatalf("same-Epoch prefix mutated at msg[%d]!\nwant: %s\ngot:  %s", i, b1[i], bSame[i])
		}
	}

	// Cold reload into a fresh AgentInstance (RPC pattern) in SAME Epoch #1 must also reuse frozen WorkingSet!
	reloaded := NewAgent("ws-frozen", "worker", prov, "claude-4-sonnet", tools.NewRegistry())
	reloaded.SetSystemPrompt("sys rules")
	reloadedTree, err := session.NewSessionTree("ws-frozen", store)
	if err != nil {
		t.Fatalf("reload SessionTree error = %v", err)
	}
	reloaded.SetSessionTree(reloadedTree)
	reloaded.SetStorage(store)
	asmReload, metaReload := reloaded.assembleModelMessagesWithMetrics()
	if metaReload.retrievalBuildMs != 0 || asmReload[2].Content != asm1[2].Content {
		t.Fatalf("expected persisted WorkingSet reused byte-identically on cold RPC reload within same Epoch")
	}

	// Push into Epoch #2 (>= 37 tool turns) with a different latest query -> WorkingSet refreshes!
	for i := 22; i < 37; i++ {
		cid := fmt.Sprintf("call_ws_%02d", i)
		payload := large
		if i == 25 {
			payload = fmt.Sprintf("config/deploy_vpc.yaml internal-registry GOPROXY=off\n%s", large)
		}
		reloadedTree.AppendMessage(ai.Message{
			Role: ai.RoleAssistant,
			ToolCalls: []ai.ToolCall{{
				ID:       cid,
				Type:     "function",
				Function: ai.FunctionCall{Name: "read", Arguments: json.RawMessage(`{}`)},
			}},
		})
		reloadedTree.AppendMessage(ai.Message{
			Role:       ai.RoleTool,
			ToolCallID: cid,
			Content:    payload,
		})
	}
	reloadedTree.AppendMessage(ai.NewUserMessage("Show me config/deploy_vpc.yaml GOPROXY setting"))
	asmEp2, metaEp2 := reloaded.assembleModelMessagesWithMetrics()
	if metaEp2.epochID != 2 || !strings.Contains(asmEp2[2].Content, "deploy_vpc.yaml") {
		t.Fatalf("expected WorkingSet to refresh for Epoch #2 query deploy_vpc.yaml, got ep=%d ws=%q", metaEp2.epochID, asmEp2[2].Content)
	}

	// Executing Compact() creates a new compactionAnchorID -> refreshes WorkingSet even within same Epoch!
	prevWS := asmEp2[2].Content
	reloadedTree.AppendMessage(ai.NewUserMessage("Re-verify AuditSlotVerifier vs reserved 12"))
	if _, err := reloaded.Compact(context.Background(), ""); err != nil {
		t.Fatalf("Compact() error = %v", err)
	}
	asmCompacted, _ := reloaded.assembleModelMessagesWithMetrics()
	if asmCompacted[2].Content == prevWS && !strings.Contains(asmCompacted[2].Content, "AuditSlotVerifier") {
		t.Fatalf("expected WorkingSet to refresh after new Compaction anchor, got %q", asmCompacted[2].Content)
	}
}

func TestWorkingSet_RecallsMaskedAndCompactedAndSuppressesSuperseded(t *testing.T) {
	store, err := sqlite.Open(t.TempDir() + "/ws_both_sources.db")
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	prov := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{{DeltaText: `{"ledger_events":[],"handoff_notes":"compacted before tool epoch"}`}}
			},
		},
	}

	ag := NewAgent("ws-both", "sys", prov, "claude-4-sonnet", tools.NewRegistry())
	ag.SetStorage(store)
	st, err := session.NewSessionTree("ws-both", store)
	if err != nil {
		t.Fatalf("NewSessionTree() error = %v", err)
	}
	ag.SetSessionTree(st)

	eSuperseded := st.AppendMessage(ai.NewUserMessage("OLD_POLICY RedisEngineV1 with shard_router_token"))
	eCompactedGood := st.AppendMessage(ai.NewAssistantMessage("ACTIVE_EVIDENCE: internal/payment/settlement_worker.go runs idempotent retry ledger checks.", ""))
	for i := 0; i < 8; i++ {
		st.AppendMessage(ai.NewUserMessage(fmt.Sprintf("pre-compact filler user %d", i)))
		st.AppendMessage(ai.NewAssistantMessage(fmt.Sprintf("pre-compact filler assistant %d", i), ""))
	}

	// Mark eSuperseded strictly under a StatusSuperseded constraint.
	ledger := compaction.NewContextLedger()
	ledger.Apply("add", "cst_engine", "tech_stack", "Use RedisEngineV1", "", []string{eSuperseded.ID}, 1)
	ledger.Apply("supersede", "cst_engine_v2", "tech_stack", "Use PostgresEngineV2", "cst_engine", []string{"new_entry"}, 2)
	ag.SetContextLedger(ledger)

	if _, err := ag.Compact(context.Background(), ""); err != nil {
		t.Fatalf("Compact() error = %v", err)
	}

	// Post-compaction: add 21 tool turns so Epoch masks earlier RoleTool output (including maskedGood).
	large := strings.Repeat("trace line filler\n", 25)
	var eMaskedGoodID string
	for i := 0; i < 21; i++ {
		cid := fmt.Sprintf("call_both_%02d", i)
		st.AppendMessage(ai.Message{
			Role: ai.RoleAssistant,
			ToolCalls: []ai.ToolCall{{
				ID:       cid,
				Type:     "function",
				Function: ai.FunctionCall{Name: "read", Arguments: json.RawMessage(`{}`)},
			}},
		})
		content := large
		if i == 1 {
			content = fmt.Sprintf("MASKED_EVIDENCE: internal/payment/settlement_worker.go ShardDeadlockGuard max_batch=64\n%s", large)
		}
		te := st.AppendMessage(ai.Message{
			Role:       ai.RoleTool,
			ToolCallID: cid,
			Content:    content,
		})
		if i == 1 {
			eMaskedGoodID = te.ID
		}
	}
	st.AppendMessage(ai.NewUserMessage("Check settlement_worker.go ShardDeadlockGuard and OLD_POLICY RedisEngineV1"))

	asm, meta := ag.assembleModelMessagesWithMetrics()
	if meta.epochMaskedCount == 0 || meta.workingSetEntries == 0 {
		t.Fatalf("expected non-zero masked count and working set entries, got %+v", meta)
	}
	var wsContent string
	for _, m := range asm {
		if strings.HasPrefix(m.Content, "<working_set>") {
			wsContent = m.Content
			break
		}
	}
	if !strings.Contains(wsContent, eCompactedGood.ID) {
		t.Fatalf("expected pre-compaction truncated entry %s in <working_set>, got:\n%s", eCompactedGood.ID, wsContent)
	}
	if !strings.Contains(wsContent, eMaskedGoodID) {
		t.Fatalf("expected Epoch-masked in-window RoleTool entry %s in <working_set>, got:\n%s", eMaskedGoodID, wsContent)
	}
	if strings.Contains(wsContent, eSuperseded.ID) || strings.Contains(wsContent, "OLD_POLICY") {
		t.Fatalf("expected StatusSuperseded provenance %s suppressed from <working_set>, got:\n%s", eSuperseded.ID, wsContent)
	}
}

func TestSearchContextTool_MultiHopAcrossCompactionAndSupersededFiltering(t *testing.T) {
	store, err := sqlite.Open(t.TempDir() + "/search_ctx.db")
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	reg := tools.NewRegistry()
	_ = reg.Register(&dummyReplayTool{name: "read", mode: tools.ReplaySafe})

	prov := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			// 1. Compact LLM response
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{{DeltaText: `{"ledger_events":[],"handoff_notes":"compacted early auth discussion"}`}}
			},
			// 2. RunLoop turn 1: assistant calls search_context
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{{
					ToolCallDelta: &ai.ToolCall{
						ID:   "call_sc_1",
						Type: "function",
						Function: ai.FunctionCall{
							Name:      "search_context",
							Arguments: json.RawMessage(`{"query":"RefreshRotationPolicy"}`),
						},
					},
				}}
			},
			// 3. RunLoop turn 2: assistant finishes
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{{DeltaText: "Verified RefreshRotationPolicy from history."}}
			},
		},
	}

	ag := NewAgent("search-agent", "worker", prov, "claude-4-sonnet", reg)
	ag.SetStorage(store)
	st, err := session.NewSessionTree("search-agent", store)
	if err != nil {
		t.Fatalf("NewSessionTree() error = %v", err)
	}
	ag.SetSessionTree(st)

	searchTool, ok := reg.Get("search_context")
	if !ok {
		t.Fatalf("expected search_context to be auto-registered in NewAgent")
	}
	if tools.GetReplayMode(searchTool) != tools.ReplaySafe {
		t.Fatalf("expected search_context ReplayMode == ReplaySafe")
	}

	// Entry A: mentions auth/token_store.go WITHOUT mentioning RefreshRotationPolicy (2-hop target).
	eHopA := st.AppendMessage(ai.NewAssistantMessage("auth/token_store.go stores signing keys in KMS envelope cipher with 15m TTL SECRET_ROTATION_KEY.", ""))
	// Entry B: bridges auth/token_store.go <-> RefreshRotationPolicy.
	_ = st.AppendMessage(ai.NewAssistantMessage("RefreshRotationPolicy invokes auth/token_store.go during zero-downtime key rollover.", ""))
	// Entry Sup: linked strictly to a superseded constraint.
	eSup := st.AppendMessage(ai.NewUserMessage("DEPRECATED RefreshRotationPolicy uses plaintext cookies"))

	for i := 0; i < 8; i++ {
		st.AppendMessage(ai.NewUserMessage(fmt.Sprintf("follow-up question %d", i)))
		st.AppendMessage(ai.NewAssistantMessage(fmt.Sprintf("follow-up answer %d", i), ""))
	}

	ledger := compaction.NewContextLedger()
	ledger.Apply("add", "cst_cookie", "auth", "Use plaintext cookies", "", []string{eSup.ID}, 1)
	ledger.Apply("supersede", "cst_kms", "auth", "Use KMS cookies", "cst_cookie", []string{eHopA.ID}, 2)
	ag.SetContextLedger(ledger)

	if _, err := ag.Compact(context.Background(), ""); err != nil {
		t.Fatalf("Compact() error = %v", err)
	}

	// 1. Default include_superseded=false finds multi-hop compacted entry A & suppresses eSup.
	resDefault, err := searchTool.Execute(&tools.ToolContext{Context: context.Background()},
		json.RawMessage(`{"query":"RefreshRotationPolicy","top_k":6}`))
	if err != nil || resDefault.IsError {
		t.Fatalf("search_context failed: err=%v res=%+v", err, resDefault)
	}
	if !strings.Contains(resDefault.Content, eHopA.ID) || !strings.Contains(resDefault.Content, "status=compacted") {
		t.Fatalf("expected search_context to find 2-hop compacted entry %s, got:\n%s", eHopA.ID, resDefault.Content)
	}
	if !strings.Contains(resDefault.Content, "recall_entries") {
		t.Fatalf("expected search_context output to instruct recall_entries for verbatim view, got:\n%s", resDefault.Content)
	}
	if strings.Contains(resDefault.Content, eSup.ID) || strings.Contains(resDefault.Content, "DEPRECATED") {
		t.Fatalf("expected superseded entry %s excluded by default, got:\n%s", eSup.ID, resDefault.Content)
	}

	// 2. recall_entries retrieves exact verbatim SECRET_ROTATION_KEY from discovered entry_id.
	recallTool, _ := reg.Get("recall_entries")
	recallArgs, _ := json.Marshal(map[string]any{"entry_ids": []string{eHopA.ID}})
	recallRes, err := recallTool.Execute(&tools.ToolContext{Context: context.Background()}, recallArgs)
	if err != nil || recallRes.IsError || !strings.Contains(recallRes.Content, "SECRET_ROTATION_KEY") {
		t.Fatalf("expected recall_entries to return verbatim SECRET_ROTATION_KEY, err=%v res=%+v", err, recallRes)
	}

	// 3. include_superseded=true reveals eSup.
	resInc, err := searchTool.Execute(&tools.ToolContext{Context: context.Background()},
		json.RawMessage(`{"query":"RefreshRotationPolicy","top_k":12,"include_superseded":true}`))
	if err != nil || resInc.IsError || !strings.Contains(resInc.Content, eSup.ID) {
		t.Fatalf("expected include_superseded=true to return %s, err=%v res=%+v", eSup.ID, err, resInc)
	}

	// 4. End-to-end RunLoop call invoking search_context records SearchCalls & totals in SQLite context_metrics.
	out, err := RunLoop(context.Background(), ag, "Find RefreshRotationPolicy details")
	if err != nil || out == "" {
		t.Fatalf("RunLoop error = %v out=%q", err, out)
	}
	totals, err := store.GetContextMetricTotals(ag.SessionID())
	if err != nil || totals.TotalSearchCalls != 1 {
		t.Fatalf("expected TotalSearchCalls=1 recorded in SQLite, got %+v err=%v", totals, err)
	}
	metrics, err := store.ListContextMetrics(ag.SessionID(), 5)
	if err != nil || len(metrics) < 2 || metrics[0].StrategyMode != strategyModeE4APR2 {
		t.Fatalf("unexpected persisted metrics: %+v err=%v", metrics, err)
	}
}

func TestWorkingSet_LegacySnapshotWithoutSchemaV2RebuildsAndStaysFrozen(t *testing.T) {
	store, err := sqlite.Open(t.TempDir() + "/legacy_ws_cache.db")
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const sessID = "ws-legacy-v1"
	st, err := session.NewSessionTree(sessID, store)
	if err != nil {
		t.Fatalf("NewSessionTree() error = %v", err)
	}

	feishuShell := "[角色人设]\n你是助手。\n[飞书上下文: chat_id=oc_abc | 发送者: ou_1 | 重要说明: 网关会自动上传并展示到飞书卡片]\n"
	large := strings.Repeat("filler line payload\n", 25)

	st.AppendMessage(ai.NewUserMessage(feishuShell + "检查 internal/agent/compact.go 里的 EpochMaskingGuard"))
	for i := 0; i < 21; i++ {
		cid := fmt.Sprintf("call_legacy_%02d", i)
		content := large
		if i == 1 {
			content = "TARGET_SYMBOL: internal/agent/compact.go defines EpochMaskingGuard with budget cap.\n" + large
		} else if i < 6 {
			// Also add multiple wrapped RoleUser entries to ensure they don't dominate via boilerplate!
			st.AppendMessage(ai.NewUserMessage(feishuShell + fmt.Sprintf("补充问题 %d", i)))
		}
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
			Content:    content,
		})
	}
	// Latest RoleUser is an empty wrapper -> latestUserQuery must fall back to earlier non-empty real question,
	// or if a new real question exists, use that without boilerplate.
	st.AppendMessage(ai.NewUserMessage(feishuShell + "请分析 EpochMaskingGuard 和 compact.go 逻辑"))
	st.AppendMessage(ai.NewUserMessage(feishuShell + "   "))

	// Seed legacy dirty working_set_cache (missing schema_version == 2, containing [角色人设]).
	dirtyLegacySnap := workingSetSnapshot{
		Initialized: true,
		EpochID:     1,
		AnchorID:    "",
		Text:        "<working_set>\n[user#old]: [角色人设] stale polluted snapshot\n</working_set>",
		Tokens:      999,
		Entries:     10,
	}
	dirtyBytes, _ := json.Marshal(dirtyLegacySnap)
	if err := store.Commit(context.Background(), func(tx storage.Transaction) error {
		return tx.SetDocument(sessID, workingSetDocKey, dirtyBytes)
	}); err != nil {
		t.Fatalf("seed legacy working_set_cache error = %v", err)
	}

	// First load: legacy snapshot without schema_version==2 must be ignored and rebuilt cleanly!
	ag := NewAgent(sessID, "worker", nil, "claude-4-sonnet", tools.NewRegistry())
	ag.SetSystemPrompt("base system prompt")
	ag.SetSessionTree(st)
	ag.SetStorage(store)

	asm1, meta1 := ag.assembleModelMessagesWithMetrics()
	if meta1.epochID != 1 || meta1.retrievalBuildMs <= 0 {
		t.Fatalf("expected rebuild on missing schema_version==2, got ep=%d buildMs=%.3f", meta1.epochID, meta1.retrievalBuildMs)
	}
	var wsContent string
	for _, m := range asm1 {
		if strings.HasPrefix(m.Content, "<working_set>") {
			wsContent = m.Content
			break
		}
	}
	if wsContent == "" {
		t.Fatalf("expected rebuilt <working_set>, got none")
	}
	if strings.Contains(wsContent, "[角色人设]") || strings.Contains(wsContent, "[飞书上下文:") || strings.Contains(wsContent, "stale polluted snapshot") {
		t.Fatalf("rebuilt <working_set> must NOT contain legacy snapshot or gateway boilerplate:\n%s", wsContent)
	}
	if !strings.Contains(wsContent, "EpochMaskingGuard") {
		t.Fatalf("expected rebuilt <working_set> to recall EpochMaskingGuard, got:\n%s", wsContent)
	}

	// Verify persisted snapshot now carries SchemaVersion == 2.
	rawPersisted, ok, err := store.GetDocument(sessID, workingSetDocKey)
	if err != nil || !ok {
		t.Fatalf("expected persisted working_set_cache after rebuild, ok=%v err=%v", ok, err)
	}
	var saved workingSetSnapshot
	if err := json.Unmarshal(rawPersisted, &saved); err != nil || saved.SchemaVersion != workingSetSchemaVersion {
		t.Fatalf("expected schema_version=%d in persisted snapshot, got %+v err=%v", workingSetSchemaVersion, saved, err)
	}

	// Subsequent turn & cold reload in same Epoch must hit SchemaVersion==2 cache with zero jitter!
	st.AppendMessage(ai.Message{
		Role: ai.RoleAssistant,
		ToolCalls: []ai.ToolCall{{
			ID:       "call_legacy_21",
			Type:     "function",
			Function: ai.FunctionCall{Name: "read", Arguments: json.RawMessage(`{}`)},
		}},
	})
	st.AppendMessage(ai.Message{
		Role:       ai.RoleTool,
		ToolCallID: "call_legacy_21",
		Content:    "next step short ok",
	})

	asmSame, metaSame := ag.assembleModelMessagesWithMetrics()
	if metaSame.retrievalBuildMs != 0 {
		t.Fatalf("expected zero-jitter cache hit after rebuild, got buildMs=%.3f", metaSame.retrievalBuildMs)
	}
	b1 := marshalWireMessageBytes(t, asm1)
	bSame := marshalWireMessageBytes(t, asmSame)
	for i := 0; i < len(b1); i++ {
		if string(b1[i]) != string(bSame[i]) {
			t.Fatalf("same-Epoch prefix mutated after v2 rebuild at msg[%d]!\nwant: %s\ngot:  %s", i, b1[i], bSame[i])
		}
	}

	reloaded := NewAgent(sessID, "worker", nil, "claude-4-sonnet", tools.NewRegistry())
	reloaded.SetSystemPrompt("base system prompt")
	reloadedTree, err := session.NewSessionTree(sessID, store)
	if err != nil {
		t.Fatalf("reload SessionTree error = %v", err)
	}
	reloaded.SetSessionTree(reloadedTree)
	reloaded.SetStorage(store)
	asmReload, metaReload := reloaded.assembleModelMessagesWithMetrics()
	if metaReload.retrievalBuildMs != 0 {
		t.Fatalf("expected persisted v2 WorkingSet cache hit on reload, got buildMs=%.3f", metaReload.retrievalBuildMs)
	}
	if len(asmReload) < 2 || asmReload[1].Content != wsContent {
		t.Fatalf("expected identical rebuilt WorkingSet after cold reload")
	}
}
