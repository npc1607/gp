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
			b.WriteString(fmt.Sprintf("%s line %d: some verbose diagnostic output payload\n", tag, i))
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
		if turn == 0 {
			content = turn0Text
		} else if turn == 1 {
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
