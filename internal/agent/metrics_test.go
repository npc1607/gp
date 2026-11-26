package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"gp/internal/ai"
	"gp/internal/core/compaction"
	"gp/internal/core/session"
	"gp/internal/core/storage/sqlite"
	"gp/internal/tools"
)

type metricsCaptureProvider struct {
	turns int
}

func (p *metricsCaptureProvider) Name() string { return "metrics-capture" }

func (p *metricsCaptureProvider) StreamChat(_ context.Context, _ *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	ch := make(chan ai.StreamChunk, 4)
	p.turns++
	turn := p.turns
	go func() {
		defer close(ch)
		switch turn {
		case 1:
			// Turn 1 triggers a recall_entries tool call
			idx := 0
			ch <- ai.StreamChunk{
				ToolCallDelta: &ai.ToolCall{
					Index: &idx,
					ID:    "call_recall_1",
					Type:  "function",
					Function: ai.FunctionCall{
						Name:      "recall_entries",
						Arguments: json.RawMessage(`{"entry_ids":["missing_entry_for_test"]}`),
					},
				},
			}
			ch <- ai.StreamChunk{
				Usage: &ai.TokenUsage{
					InputTokens:      1200,
					CacheReadTokens:  800,
					CacheWriteTokens: 400,
					OutputTokens:     50,
				},
				FinishReason: "tool_calls",
			}
		default:
			// Turn 2 returns final text
			ch <- ai.StreamChunk{DeltaText: "done"}
			ch <- ai.StreamChunk{
				Usage: &ai.TokenUsage{
					InputTokens:      1350,
					CacheReadTokens:  1200,
					CacheWriteTokens: 0,
					OutputTokens:     30,
				},
				FinishReason: "stop",
			}
		}
	}()
	return ch, nil
}

func TestAgentLoop_PersistsContextMetricsAndLogsStructuredEvents(t *testing.T) {
	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "agent_metrics.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	reg := tools.NewRegistry()
	ag := NewAgent("metrics-agent", "worker", &metricsCaptureProvider{}, "claude-4-sonnet", reg)
	ag.SetStorage(store)
	ag.SetChannelID("feishu:oc_test_chat")
	st, err := session.NewSessionTree("metrics-agent", store)
	if err != nil {
		t.Fatalf("NewSessionTree() error = %v", err)
	}
	ag.SetSessionTree(st)

	// Trigger ContextLedger mutations (add + supersede) so ledger_hash & [ledger_diff] are exercised
	diffs := ag.ApplyLedgerEvents(context.Background(), []compaction.LedgerEvent{
		{
			Action:     "add",
			ID:         "cst_db",
			Category:   "decision",
			Statement:  "Use PostgreSQL",
			Provenance: []string{"e_1"},
		},
		{
			Action:       "supersede",
			ID:           "cst_db_v2",
			Category:     "decision",
			Statement:    "Use SQLite WAL mode",
			SupersedesID: "cst_db",
			Provenance:   []string{"e_2"},
		},
	})
	if len(diffs) != 2 {
		t.Fatalf("expected 2 ledger diffs, got %d", len(diffs))
	}

	reply, err := RunLoop(context.Background(), ag, "check entry missing_entry_for_test")
	if err != nil {
		t.Fatalf("RunLoop() error = %v", err)
	}
	if reply != "done" {
		t.Fatalf("unexpected reply %q", reply)
	}

	metrics, err := store.ListContextMetrics("metrics-agent", 10)
	if err != nil {
		t.Fatalf("ListContextMetrics() error = %v", err)
	}
	if len(metrics) != 2 {
		t.Fatalf("expected 2 context_metrics rows (tool turn + final turn), got %d", len(metrics))
	}

	// Newest-first: index 0 is turn 2 (final), index 1 is turn 1 (tool call)
	turn2 := metrics[0]
	turn1 := metrics[1]
	if turn1.TurnIndex != 1 || turn1.RecallCalls != 1 || turn1.InputTokens != 1200 || turn1.CacheReadTokens != 800 || turn1.CacheWriteTokens != 400 {
		t.Fatalf("unexpected turn1 metric: %+v", turn1)
	}
	if turn2.TurnIndex != 2 || turn2.RecallCalls != 0 || turn2.InputTokens != 1350 || turn2.CacheReadTokens != 1200 {
		t.Fatalf("unexpected turn2 metric: %+v", turn2)
	}
	if turn2.ChannelID != "feishu:oc_test_chat" || turn2.StrategyMode != strategyModeE4APR2 {
		t.Fatalf("unexpected channel/strategy: %+v", turn2)
	}
	if turn2.LedgerVersion != 2 || turn2.LedgerActiveCnt != 1 || turn2.LedgerSuperseded != 1 || len(turn2.LedgerHash) != 8 {
		t.Fatalf("unexpected ledger snapshot in metric: %+v", turn2)
	}

	totals, err := store.GetContextMetricTotals("metrics-agent")
	if err != nil {
		t.Fatalf("GetContextMetricTotals() error = %v", err)
	}
	if totals.TurnCount != 2 || totals.TotalRecallCalls != 1 || totals.TotalRestoreCalls != 0 {
		t.Fatalf("unexpected totals: %+v", totals)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "[ledger_diff]") || !strings.Contains(logs, "action=add") || !strings.Contains(logs, "action=supersede") {
		t.Fatalf("expected [ledger_diff] audit logs, got:\n%s", logs)
	}
	if !strings.Contains(logs, "[ctx_metrics]") || !strings.Contains(logs, "ledger_hash="+turn2.LedgerHash) {
		t.Fatalf("expected [ctx_metrics] structured log, got:\n%s", logs)
	}
	if !strings.Contains(logs, "sys_tokens=") || !strings.Contains(logs, "tools_tokens=") || !strings.Contains(logs, "ledger_tokens=") || !strings.Contains(logs, "msgs_tokens=") ||
		!strings.Contains(logs, "working_set_tokens=") || !strings.Contains(logs, "working_set_entries=") || !strings.Contains(logs, "retrieval_build_ms=") || !strings.Contains(logs, "graph_matched_entities=") || !strings.Contains(logs, "search_calls=") {
		t.Fatalf("expected token distribution and PR #2 retrieval fields in [ctx_metrics], got:\n%s", logs)
	}
}

func TestCompact_FreezesLedgerWithinSameEpochAndUpdatesOnNextEpoch(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "epoch_freeze.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	callSeq := 0
	var capturedPrompts []string
	prov := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			// Compact #1 (Epoch 0): allowed to add cst_initial
			func(req *ai.ChatRequest) []ai.StreamChunk {
				callSeq++
				capturedPrompts = append(capturedPrompts, req.Messages[len(req.Messages)-1].Content)
				return []ai.StreamChunk{{DeltaText: `{"ledger_events":[{"action":"add","id":"cst_initial","category":"constraint","statement":"Use Go 1.24 only","provenance":["e1"]}],"handoff_notes":"Epoch 0 summary"}`}}
			},
			// Compact #2 (still Epoch 0): prompt freezes ledger and any rogue ledger_events are ignored
			func(req *ai.ChatRequest) []ai.StreamChunk {
				callSeq++
				capturedPrompts = append(capturedPrompts, req.Messages[len(req.Messages)-1].Content)
				return []ai.StreamChunk{{DeltaText: `{"ledger_events":[{"action":"add","id":"cst_should_be_ignored","category":"api","statement":"Rogue noise","provenance":["e2"]}],"handoff_notes":"Same epoch handoff"}`}}
			},
			// Compact #3 (Epoch 1): allowed to update ledger
			func(req *ai.ChatRequest) []ai.StreamChunk {
				callSeq++
				capturedPrompts = append(capturedPrompts, req.Messages[len(req.Messages)-1].Content)
				return []ai.StreamChunk{{DeltaText: `{"ledger_events":[{"action":"add","id":"cst_epoch1","category":"decision","statement":"Enable WAL checkpoints","provenance":["e3"]}],"handoff_notes":"Epoch 1 summary"}`}}
			},
		},
	}

	ag := NewAgent("freeze-epoch-agent", "worker", prov, "claude-4-sonnet", tools.NewRegistry())
	ag.SetStorage(store)
	st, err := session.NewSessionTree("freeze-epoch-agent", store)
	if err != nil {
		t.Fatalf("NewSessionTree() error = %v", err)
	}
	ag.SetSessionTree(st)

	// Append 8 messages (Epoch 0: recentCutoff < 16)
	for i := 0; i < 4; i++ {
		st.AppendMessage(ai.NewUserMessage(fmt.Sprintf("user turn %d", i)))
		st.AppendMessage(ai.NewAssistantMessage(fmt.Sprintf("assistant turn %d", i), ""))
	}

	res1, err := ag.Compact(context.Background(), "")
	if err != nil {
		t.Fatalf("Compact #1 error = %v", err)
	}
	ver1, active1, _, hash1 := res1.Ledger.Stats()
	if ver1 != 1 || active1 != 1 {
		t.Fatalf("expected Compact #1 to add 1 ledger constraint, got ver=%d active=%d", ver1, active1)
	}

	// Verify tightened prompt forbids system prompt & code reading trivia
	if !strings.Contains(capturedPrompts[0], "FORBIDDEN in ledger_events") {
		t.Fatalf("expected tightened compaction rules in prompt, got:\n%s", capturedPrompts[0])
	}

	// Append 2 more messages -> still inside Epoch 0; reload agent as RPC does on each turn!
	st.AppendMessage(ai.NewUserMessage("follow up within epoch 0"))
	st.AppendMessage(ai.NewAssistantMessage("follow up reply", ""))

	reloaded := NewAgent("freeze-epoch-agent", "worker", prov, "claude-4-sonnet", tools.NewRegistry())
	reloaded.SetStorage(store)
	reloadedTree, err := session.NewSessionTree("freeze-epoch-agent", store)
	if err != nil {
		t.Fatalf("reload SessionTree error = %v", err)
	}
	reloaded.SetSessionTree(reloadedTree)

	res2, err := reloaded.Compact(context.Background(), "")
	if err != nil {
		t.Fatalf("Compact #2 error = %v", err)
	}
	ver2, active2, _, hash2 := res2.Ledger.Stats()
	if ver2 != ver1 || active2 != active1 || hash2 != hash1 {
		t.Fatalf("Ledger MUST stay frozen within Epoch 0! before=(v%d,%d,%s) after=(v%d,%d,%s)", ver1, active1, hash1, ver2, active2, hash2)
	}
	if !strings.Contains(capturedPrompts[1], "FROZEN for the current Epoch") {
		t.Fatalf("expected frozen ledger instruction in prompt #2, got:\n%s", capturedPrompts[1])
	}

	// Advance past Epoch 1 boundary (>= 16+window = 21 tool-calling turns total)
	for i := 0; i < 21; i++ {
		cid := fmt.Sprintf("call_ep1_%02d", i)
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
			Content:    fmt.Sprintf("ok %d", i),
		})
	}
	res3, err := reloaded.Compact(context.Background(), "")
	if err != nil {
		t.Fatalf("Compact #3 error = %v", err)
	}
	ver3, active3, _, hash3 := res3.Ledger.Stats()
	if ver3 != 2 || active3 != 2 || hash3 == hash2 {
		t.Fatalf("expected Ledger to unlock and update on Epoch 1 boundary, got v%d active=%d hash=%s", ver3, active3, hash3)
	}
}

func TestEpochObservationMasking_MonotonicAcrossCompactionAndGiantSingleTool(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "epoch_monotonic.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	st, err := session.NewSessionTree("monotonic-epoch", store)
	if err != nil {
		t.Fatalf("NewSessionTree() error = %v", err)
	}

	largeOut := strings.Repeat("giant tool output line payload\n", 40) // > 400 bytes & > 8 lines

	// 1. Single giant tool call followed by long non-tool Q&A tail (27 msgs >= 16+2*5) triggers fallback folding.
	st.AppendMessage(ai.NewUserMessage("read big file once"))
	st.AppendMessage(ai.Message{
		Role: ai.RoleAssistant,
		ToolCalls: []ai.ToolCall{{
			ID:       "call_giant_0",
			Type:     "function",
			Function: ai.FunctionCall{Name: "read", Arguments: json.RawMessage(`{}`)},
		}},
	})
	st.AppendMessage(ai.Message{
		Role:       ai.RoleTool,
		ToolCallID: "call_giant_0",
		Content:    largeOut,
	})
	for i := 0; i < 12; i++ {
		st.AppendMessage(ai.NewUserMessage(fmt.Sprintf("q %d", i)))
		st.AppendMessage(ai.NewAssistantMessage(fmt.Sprintf("a %d", i), ""))
	}

	raw1, off1 := st.GetLinearContextWithToolOffset()
	if off1 != 0 {
		t.Fatalf("expected priorToolTurnOffset=0 before compaction, got %d", off1)
	}
	masked1, _, _, folded1, _ := ApplyEpochObservationMaskingWithOffset(raw1, DefaultObservationMaskEpoch, DefaultObservationMaskWindow, off1)
	if folded1 < 1 {
		t.Fatalf("expected single giant tool output to fold via fallback once conversation grows long, got folded=%d", folded1)
	}
	if !strings.Contains(masked1[2].Content, "[已折叠历史工具输出:") {
		t.Fatalf("expected call_giant_0 to be folded, got %q", masked1[2].Content)
	}

	// 2. Append 20 more tool turns (total = 21 tool turns -> crosses Epoch 1 by tool-turn stride).
	for i := 0; i < 20; i++ {
		cid := fmt.Sprintf("call_pre_%02d", i)
		st.AppendMessage(ai.Message{
			Role: ai.RoleAssistant,
			ToolCalls: []ai.ToolCall{{
				ID:       cid,
				Type:     "function",
				Function: ai.FunctionCall{Name: "bash", Arguments: json.RawMessage(`{}`)},
			}},
		})
		st.AppendMessage(ai.Message{
			Role:       ai.RoleTool,
			ToolCallID: cid,
			Content:    largeOut,
		})
	}
	rawPre, offPre := st.GetLinearContextWithToolOffset()
	_, epPre, _, _, _ := ApplyEpochObservationMaskingWithOffset(rawPre, DefaultObservationMaskEpoch, DefaultObservationMaskWindow, offPre)
	if epPre != 1 {
		t.Fatalf("expected Epoch 1 at 21 tool turns, got %d", epPre)
	}

	// Simulate compaction truncating linear context down to the last 6 messages (3 kept tool turns -> priorToolTurnOffset = 18).
	linearBefore := st.GetLinearContext()
	kept := linearBefore[len(linearBefore)-6:]
	st.AppendCompaction("Compacted earlier 21 tool turns", kept)

	// Append 2 more tool turns after compaction (total = 23 tool turns -> Epoch 1 continues monotonically).
	for i := 0; i < 2; i++ {
		cid := fmt.Sprintf("call_post_%d", i)
		st.AppendMessage(ai.Message{
			Role: ai.RoleAssistant,
			ToolCalls: []ai.ToolCall{{
				ID:       cid,
				Type:     "function",
				Function: ai.FunctionCall{Name: "bash", Arguments: json.RawMessage(`{}`)},
			}},
		})
		st.AppendMessage(ai.Message{
			Role:       ai.RoleTool,
			ToolCallID: cid,
			Content:    largeOut,
		})
	}

	raw2, off2 := st.GetLinearContextWithToolOffset()
	if off2 != 18 {
		t.Fatalf("expected priorToolTurnOffset=18 after compaction, got %d", off2)
	}
	_, ep2, _, _, pos2 := ApplyEpochObservationMaskingWithOffset(raw2, DefaultObservationMaskEpoch, DefaultObservationMaskWindow, off2)
	if ep2 != 1 || pos2 != 2 {
		t.Fatalf("expected Epoch=1 pos=2 after compaction (never reset to 0), got ep=%d off=%d pos=%d", ep2, off2, pos2)
	}

	// Append 14 more tool turns -> total = 37 tool turns (cutoff = 32 -> crosses into Epoch 2!).
	for i := 2; i < 16; i++ {
		cid := fmt.Sprintf("call_post_%d", i)
		st.AppendMessage(ai.Message{
			Role: ai.RoleAssistant,
			ToolCalls: []ai.ToolCall{{
				ID:       cid,
				Type:     "function",
				Function: ai.FunctionCall{Name: "bash", Arguments: json.RawMessage(`{}`)},
			}},
		})
		st.AppendMessage(ai.Message{
			Role:       ai.RoleTool,
			ToolCallID: cid,
			Content:    largeOut,
		})
	}
	raw3, off3 := st.GetLinearContextWithToolOffset()
	_, ep3, fb3, folded3, pos3 := ApplyEpochObservationMaskingWithOffset(raw3, DefaultObservationMaskEpoch, DefaultObservationMaskWindow, off3)
	if ep3 != 2 || pos3 != 0 || fb3 <= 0 || folded3 <= 0 {
		t.Fatalf("expected advance to Epoch 2 after 14 more post-compaction tool turns, got ep=%d pos=%d fb=%d folded=%d", ep3, pos3, fb3, folded3)
	}
}
