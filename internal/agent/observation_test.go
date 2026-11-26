package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gp/internal/ai"
	"gp/internal/tools"
)

func TestObservationMaskingWindowAndSessionIntegrity(t *testing.T) {
	reg := tools.NewRegistry()
	a := NewAgent("obs-test", "worker", nil, "gpt-4o", reg)
	st := a.sessionTree

	st.AppendMessage(ai.NewUserMessage("排查历史任务"))

	// Build 7 tool-calling turns:
	// Turn 0: large grep output (should be masked)
	// Turn 1: tiny "ok" output (should be exempted even though outside window)
	// Turns 2..6 (most recent 5): large outputs (should remain unmasked)
	largePayload := func(tag string, lines int) string {
		var b strings.Builder
		for i := 1; i <= lines; i++ {
			fmt.Fprintf(&b, "%s line %d: some verbose diagnostic output payload\n", tag, i)
		}
		return strings.TrimRight(b.String(), "\n")
	}

	turn0Text := largePayload("turn0", 30)
	for turn := 0; turn < 7; turn++ {
		callID := fmt.Sprintf("call_%d", turn)
		toolName := "grep"
		if turn == 1 {
			toolName = "write"
		}
		st.AppendMessage(ai.Message{
			Role: ai.RoleAssistant,
			ToolCalls: []ai.ToolCall{{
				ID:       callID,
				Type:     "function",
				Function: ai.FunctionCall{Name: toolName, Arguments: json.RawMessage(`{}`)},
			}},
		})
		content := largePayload(fmt.Sprintf("turn%d", turn), 30)
		switch turn {
		case 0:
			content = turn0Text
		case 1:
			content = "ok"
		}
		st.AppendMessage(ai.Message{
			Role:       ai.RoleTool,
			ToolCallID: callID,
			Content:    content,
		})
	}

	raw := a.buildModelMessages()
	masked := applyObservationMasking(raw, 5)

	findTool := func(msgs []ai.Message, callID string) string {
		for _, m := range msgs {
			if m.Role == ai.RoleTool && m.ToolCallID == callID {
				return m.Content
			}
		}
		return ""
	}

	// Turn 0 (outside 5-turn window, large) -> masked
	got0 := findTool(masked, "call_0")
	if !strings.Contains(got0, "[已折叠历史工具输出: tool=grep call_id=call_0") || !strings.Contains(got0, "restore_observation") {
		t.Fatalf("expected turn 0 to be masked with DTOC hint, got: %q", got0)
	}

	// Turn 1 (outside 5-turn window, tiny "ok") -> exempted
	if got1 := findTool(masked, "call_1"); got1 != "ok" {
		t.Fatalf("expected tiny output in turn 1 to remain unmasked, got: %q", got1)
	}

	// Turns 2..6 (recent 5 turns) -> unmasked
	for turn := 2; turn < 7; turn++ {
		cid := fmt.Sprintf("call_%d", turn)
		got := findTool(masked, cid)
		if strings.Contains(got, "[已折叠历史工具输出") || !strings.Contains(got, fmt.Sprintf("turn%d line 30", turn)) {
			t.Fatalf("expected recent turn %d (%s) to stay unmasked, got: %q", turn, cid, got)
		}
	}

	// SessionTree original payload must remain untouched
	var persisted0 string
	for _, msg := range st.GetLinearContext() {
		if msg.Role == ai.RoleTool && msg.ToolCallID == "call_0" {
			persisted0 = msg.Content
			break
		}
	}
	if persisted0 != turn0Text {
		t.Fatalf("SessionTree was mutated! expected original turn0Text, got len=%d", len(persisted0))
	}
}

func TestRestoreObservationTool(t *testing.T) {
	dummyTool := &dummyReplayTool{name: "bash", mode: tools.ReplayInterrupt}
	reg := tools.NewRegistry()
	_ = reg.Register(dummyTool)

	a := NewAgent("obs-restore", "worker", nil, "gpt-4o", reg)
	if _, ok := reg.Get("restore_observation"); !ok {
		t.Fatal("expected restore_observation tool to be auto-registered when registry is non-empty")
	}

	var lines []string
	for i := 1; i <= 250; i++ {
		lines = append(lines, fmt.Sprintf("line-%03d: payload", i))
	}
	a.sessionTree.AppendMessage(ai.Message{
		Role:       ai.RoleTool,
		ToolCallID: "call_hist_1",
		Content:    strings.Join(lines, "\n"),
	})

	rt, _ := reg.Get("restore_observation")
	if tools.GetReplayMode(rt) != tools.ReplaySafe {
		t.Fatalf("expected restore_observation to be ReplaySafe")
	}

	// 1. Slice lines 10..20
	res, err := rt.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{"call_id":"call_hist_1","start_line":10,"end_line":20}`))
	if err != nil || res.IsError {
		t.Fatalf("unexpected restore error: err=%v res=%+v", err, res)
	}
	if !strings.Contains(res.Content, "第 10-20 行，共 250 行") || !strings.Contains(res.Content, "line-010: payload") || !strings.Contains(res.Content, "line-020: payload") || strings.Contains(res.Content, "line-021: payload") {
		t.Fatalf("unexpected slice content: %s", res.Content)
	}

	// 2. Missing call_id
	resMissing, _ := rt.Execute(&tools.ToolContext{Context: context.Background()}, json.RawMessage(`{"call_id":"nonexistent"}`))
	if !resMissing.IsError || !strings.Contains(resMissing.Content, "未在当前会话历史中找到") {
		t.Fatalf("expected error result for missing call_id, got: %+v", resMissing)
	}
}

type dummyReplayTool struct {
	name string
	mode tools.ReplayMode
}

func (d *dummyReplayTool) Name() string                 { return d.name }
func (d *dummyReplayTool) Description() string          { return "dummy" }
func (d *dummyReplayTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (d *dummyReplayTool) ReplayMode() tools.ReplayMode { return d.mode }
func (d *dummyReplayTool) Execute(_ *tools.ToolContext, _ json.RawMessage) (*tools.ToolResult, error) {
	return &tools.ToolResult{Content: "ok"}, nil
}

func TestEpochObservationMasking_FreezesWithinEpoch(t *testing.T) {
	large := strings.Repeat("verbose line payload\n", 25)
	var msgs []ai.Message
	// Build 36 tool-calling turns (72 messages), interspersed with user text messages.
	for turn := 0; turn < 36; turn++ {
		if turn%4 == 0 {
			msgs = append(msgs, ai.NewUserMessage(fmt.Sprintf("step %d", turn)))
		}
		cid := fmt.Sprintf("c_%02d", turn)
		msgs = append(msgs,
			ai.Message{
				Role: ai.RoleAssistant,
				ToolCalls: []ai.ToolCall{{
					ID:       cid,
					Type:     "function",
					Function: ai.FunctionCall{Name: "bash", Arguments: json.RawMessage(`{}`)},
				}},
			},
			ai.Message{Role: ai.RoleTool, ToolCallID: cid, Content: fmt.Sprintf("t=%d\n%s", turn, large)},
		)
	}

	// Find slice end indices immediately after tool turn N completes (0-indexed turn count = N).
	endAfterTurn := func(turns int) int {
		seen := 0
		for i, m := range msgs {
			if m.Role == ai.RoleAssistant && len(m.ToolCalls) > 0 {
				seen++
				if seen == turns {
					return i + 2 // include assistant + tool response
				}
			}
		}
		return len(msgs)
	}

	// Turn 20 (cutoff = 15 < 16): stays in Epoch 0 with 0 tool outputs masked.
	_, ep20, fb20, masked20, pos20 := ApplyEpochObservationMaskingWithOffset(msgs[:endAfterTurn(20)], 16, 5, 0)
	if ep20 != 0 || fb20 != 0 || masked20 != 0 || pos20 != 15 {
		t.Fatalf("expected turn 20 in Epoch 0 (fb=0 masked=0 pos=15), got ep=%d fb=%d masked=%d pos=%d", ep20, fb20, masked20, pos20)
	}

	// Turns 21, 25, 36 (cutoffs = 16, 20, 31): all stay in Epoch 1 with identical frozen boundary (turns 0..15).
	m21, ep21, fb21, masked21, pos21 := ApplyEpochObservationMaskingWithOffset(msgs[:endAfterTurn(21)], 16, 5, 0)
	m25, ep25, fb25, _, pos25 := ApplyEpochObservationMaskingWithOffset(msgs[:endAfterTurn(25)], 16, 5, 0)
	m36, ep36, fb36, masked36, pos36 := ApplyEpochObservationMaskingWithOffset(msgs[:endAfterTurn(36)], 16, 5, 0)

	if ep21 != 1 || ep25 != 1 || ep36 != 1 || fb21 != fb25 || fb21 != fb36 {
		t.Fatalf("expected all of turns 21/25/36 frozen in Epoch 1 with same freezeBoundary, got ep=(%d,%d,%d) fb=(%d,%d,%d)", ep21, ep25, ep36, fb21, fb25, fb36)
	}
	if pos21 != 0 || pos25 != 4 || pos36 != 15 || masked21 != 16 || masked36 != 16 {
		t.Fatalf("unexpected epoch stats: pos=(%d,%d,%d) masked=(%d,%d)", pos21, pos25, pos36, masked21, masked36)
	}

	for i := 0; i < len(m21); i++ {
		if m21[i].Content != m25[i].Content || m21[i].Content != m36[i].Content {
			t.Fatalf("expected frozen prefix at msg %d across epoch 1, got diffs", i)
		}
	}
	// Recent 5 tool outputs (turns 31..35) must remain unmasked in m36.
	toolTailSeen := 0
	for i := len(m36) - 1; i >= 0 && toolTailSeen < 5; i-- {
		if m36[i].Role == ai.RoleTool {
			toolTailSeen++
			if strings.Contains(m36[i].Content, "[已折叠历史工具输出") {
				t.Fatalf("expected recent tool msg %d to remain unmasked", i)
			}
		}
	}
}
