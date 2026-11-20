package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gp/internal/ai"
	"gp/internal/core/storage/sqlite"
	"gp/internal/tools"
)

type safeDelayedTool struct {
	name  string
	delay time.Duration
	calls atomic.Int32
}

func (t *safeDelayedTool) Name() string        { return t.name }
func (t *safeDelayedTool) Description() string { return "safe delayed test tool" }
func (t *safeDelayedTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t *safeDelayedTool) ReplayMode() tools.ReplayMode { return tools.ReplaySafe }
func (t *safeDelayedTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	t.calls.Add(1)
	if t.delay > 0 {
		time.Sleep(t.delay)
	}
	return &tools.ToolResult{Content: "ok:" + t.name + ":" + string(rawArgs)}, nil
}

type largeOutputTool struct {
	payload string
}

func (t *largeOutputTool) Name() string        { return "large_out" }
func (t *largeOutputTool) Description() string { return "returns oversized output" }
func (t *largeOutputTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t *largeOutputTool) ReplayMode() tools.ReplayMode { return tools.ReplaySafe }
func (t *largeOutputTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	return &tools.ToolResult{Content: t.payload}, nil
}

func TestAgentLoop_NilToolCallIndexFallbackAndConcurrentSafeTools(t *testing.T) {
	toolA := &safeDelayedTool{name: "safe_a", delay: 60 * time.Millisecond}
	toolB := &safeDelayedTool{name: "safe_b", delay: 60 * time.Millisecond}

	reg := tools.NewRegistry()
	_ = reg.Register(toolA)
	_ = reg.Register(toolB)

	mock := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			func(req *ai.ChatRequest) []ai.StreamChunk {
				// 刻意不提供 Index (nil)，模拟非 OpenAI 兼容网关流式返回多个并发 tool call
				return []ai.StreamChunk{
					{ToolCallDelta: &ai.ToolCall{ID: "c_a", Function: ai.FunctionCall{Name: "safe_a", Arguments: json.RawMessage(`{"x":`)}}},
					{ToolCallDelta: &ai.ToolCall{ID: "c_a", Function: ai.FunctionCall{Arguments: json.RawMessage(`1}`)}}},
					{ToolCallDelta: &ai.ToolCall{ID: "c_b", Function: ai.FunctionCall{Name: "safe_b", Arguments: json.RawMessage(`{"y":2}`)}}},
				}
			},
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{{DeltaText: "both done"}}
			},
		},
	}

	ag := NewAgent("test-nil-idx", "assistant", mock, "mock-model", reg)
	start := time.Now()
	out, err := ag.Prompt(context.Background(), "run both")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
	if out != "both done" {
		t.Fatalf("Prompt() = %q, want 'both done'", out)
	}
	if toolA.calls.Load() != 1 || toolB.calls.Load() != 1 {
		t.Fatalf("expected both tools called once, got A=%d B=%d", toolA.calls.Load(), toolB.calls.Load())
	}
	// 并发执行耗时应明显小于串行 120ms
	if elapsed >= 115*time.Millisecond {
		t.Fatalf("expected concurrent ReplaySafe tools (<115ms), took %v", elapsed)
	}
}

func TestAgentLoop_TruncatesOversizedOutputAndSkipsSafeCheckpoint(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := sqlite.Open(filepath.Join(tmpDir, "cp.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	reg := tools.NewRegistry()
	_ = reg.Register(&largeOutputTool{payload: strings.Repeat("A", 128*1024)})

	mock := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{
					{ToolCallDelta: &ai.ToolCall{ID: "c_big", Function: ai.FunctionCall{Name: "large_out", Arguments: json.RawMessage(`{}`)}}},
				}
			},
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{{DeltaText: "ok"}}
			},
		},
	}

	ag := NewAgent("test-trunc", "assistant", mock, "mock-model", reg)
	ag.SetStorage(store)

	if _, err := ag.Prompt(context.Background(), "big"); err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}

	// 验证 ReplaySafe 工具未在 SQLite 写入热路径 Checkpoint
	if _, ok := ag.GetTaskTree().GetTask("task_tool_c_big"); ok {
		t.Fatalf("expected ReplaySafe tool not to create SQLite task checkpoint")
	}

	// 验证工具输出已在 SessionTree 中被截断
	msgs := ag.GetSessionTree().GetLinearContext()
	foundTruncated := false
	for _, m := range msgs {
		if m.Role == ai.RoleTool && strings.Contains(m.Content, "[tool output truncated:") {
			foundTruncated = true
			if len(m.Content) > maxToolResultBytes+256 {
				t.Fatalf("truncated content too large: %d", len(m.Content))
			}
		}
	}
	if !foundTruncated {
		t.Fatalf("expected truncated marker in RoleTool message")
	}
}

func TestAgentLoop_PersistsThinkingSignature(t *testing.T) {
	var seenSig string
	mock := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{
					{ThinkingDelta: "step by step"},
					{ThinkingSignature: "sig-xyz-123"},
					{DeltaText: "first reply"},
				}
			},
			func(req *ai.ChatRequest) []ai.StreamChunk {
				for _, m := range req.Messages {
					if m.Role == ai.RoleAssistant && m.ThinkingSignature != "" {
						seenSig = m.ThinkingSignature
					}
				}
				return []ai.StreamChunk{{DeltaText: "second reply"}}
			},
		},
	}

	ag := NewAgent("test-sig", "assistant", mock, "mock-model", tools.NewRegistry())
	if _, err := ag.Prompt(context.Background(), "q1"); err != nil {
		t.Fatalf("Prompt(q1) error = %v", err)
	}
	if _, err := ag.Prompt(context.Background(), "q2"); err != nil {
		t.Fatalf("Prompt(q2) error = %v", err)
	}
	if seenSig != "sig-xyz-123" {
		t.Fatalf("expected ThinkingSignature 'sig-xyz-123' replayed on next turn, got %q", seenSig)
	}
}
