package compaction

import (
	"context"
	"strings"
	"testing"

	"gp/internal/ai"
)

type mockCompactProvider struct {
	summary string
	err     error
}

func (m *mockCompactProvider) Name() string { return "mock" }

func (m *mockCompactProvider) StreamChat(ctx context.Context, req *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	if m.err != nil {
		return nil, m.err
	}
	ch := make(chan ai.StreamChunk, 1)
	ch <- ai.StreamChunk{DeltaText: m.summary}
	close(ch)
	return ch, nil
}

func TestResolveContextWindow(t *testing.T) {
	cases := []struct {
		model string
		want  int64
	}{
		{"antigravity/gemini-4-argon", 1000000},
		{"qoder/gmodel", 1000000},
		{"qoder/qmodel_latest", 1000000},
		{"qoder/kmodel_latest", 256000},
		{"qoder/cmodel", 200000},
		{"qoder/ultimate", 200000},
		{"qoder/dmodel", 128000},
		{"claude-4-sonnet", 200000},
		{"deepseek-chat", 128000},
		{"gpt-4.1", 1000000},
		{"unknown-model", 128000},
	}
	for _, tc := range cases {
		if got := ResolveContextWindow(tc.model); got != tc.want {
			t.Errorf("ResolveContextWindow(%q) = %d, want %d", tc.model, got, tc.want)
		}
	}
}

func TestPruneToolOutputsAndCompact(t *testing.T) {
	largeOutput := strings.Repeat("line of log output\n", 100)
	var msgs []ai.Message
	for i := range 6 {
		msgs = append(msgs,
			ai.Message{Role: ai.RoleUser, Content: "step task"},
			ai.Message{Role: ai.RoleAssistant, Content: "running tool", ToolCalls: []ai.ToolCall{{ID: "c1", Function: ai.FunctionCall{Name: "read"}}}},
			ai.Message{Role: ai.RoleTool, ToolCallID: "c1", Content: largeOutput},
			ai.Message{Role: ai.RoleAssistant, Content: "done step"},
		)
		_ = i
	}

	pruned, count := PruneToolOutputs(msgs, 2)
	if count != 4 {
		t.Fatalf("PruneToolOutputs pruned = %d, want 4", count)
	}
	if !strings.Contains(pruned[2].Content, "[compacted tool output:") {
		t.Fatalf("expected old tool output to be compacted, got %q", pruned[2].Content)
	}
	if pruned[len(pruned)-2].Content != largeOutput {
		t.Fatalf("expected recent turn tool output to remain intact")
	}

	prov := &mockCompactProvider{summary: "1. Goal\n- Test\n2. Files\n- a.go"}
	res, err := Compact(context.Background(), prov, "claude-4-sonnet", msgs, Options{
		KeepRecentTurns:    2,
		KeepRecentMessages: 4,
	})
	if err != nil {
		t.Fatalf("Compact() error = %v", err)
	}
	if !res.UsedLLM {
		t.Fatalf("expected UsedLLM=true")
	}
	if res.AfterTokens >= res.BeforeTokens {
		t.Fatalf("expected AfterTokens (%d) < BeforeTokens (%d)", res.AfterTokens, res.BeforeTokens)
	}
}
