package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gp/internal/ai"
	"gp/internal/tools"
	"gp/internal/tools/builtin"
)

type mockProvider struct {
	mu           sync.Mutex
	turnCount    int
	chatHandlers []func(req *ai.ChatRequest) []ai.StreamChunk
}

func (m *mockProvider) Name() string { return "mock" }

func (m *mockProvider) StreamChat(ctx context.Context, req *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	idx := m.turnCount
	m.turnCount++

	if idx >= len(m.chatHandlers) {
		return nil, fmt.Errorf("no more mock responses for turn %d", idx)
	}

	chunks := m.chatHandlers[idx](req)
	ch := make(chan ai.StreamChunk, len(chunks))
	for _, c := range chunks {
		ch <- c
	}
	close(ch)
	return ch, nil
}

func setupAgentTestFile(t *testing.T, dir, filename, content string) string {
	t.Helper()
	filePath := filepath.Join(dir, filename)
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatalf("setupAgentTestFile(filename=%s): got error %v, want nil", filename, err)
	}
	t.Cleanup(func() {
		_ = os.Remove(filePath)
	})
	return filePath
}

func TestAgentLoop_MultiTurnReAct(t *testing.T) {
	fn := "AgentInstance.Prompt"
	tmpDir := t.TempDir()
	filePath := setupAgentTestFile(t, tmpDir, "secret.txt", "password123\n")

	reg, err := builtin.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("builtin.NewDefaultRegistry(): got error %v, want nil", err)
	}

	mock := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			// Turn 0: Model requests `read` tool
			func(req *ai.ChatRequest) []ai.StreamChunk {
				idx := 0
				return []ai.StreamChunk{
					{
						ToolCallDelta: &ai.ToolCall{
							Index: &idx,
							ID:    "call_read_1",
							Type:  "function",
							Function: ai.FunctionCall{
								Name:      "read",
								Arguments: json.RawMessage(fmt.Sprintf(`{"path":"%s"}`, filePath)),
							},
						},
					},
				}
			},
			// Turn 1: Model receives tool result and provides final text
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{
					{DeltaText: "The file secret is "},
					{DeltaText: "password123."},
				}
			},
		},
	}

	ag := NewAgent("test-agent", "coder", mock, "test-model", reg)
	ag.SetCwd(tmpDir)

	var streamedOutput string
	ag.SetStreamHandler(func(c ai.StreamChunk) {
		streamedOutput += c.DeltaText
	})

	ctx := context.Background()
	userInput := "What is inside secret.txt?"
	ans, err := ag.Prompt(ctx, userInput)
	if err != nil {
		t.Fatalf("%s(userInput=%q): got error %v, want nil", fn, userInput, err)
	}

	expectedAnswer := "The file secret is password123."
	if got, want := ans, expectedAnswer; got != want {
		t.Fatalf("%s(userInput=%q): got answer %q, want %q", fn, userInput, got, want)
	}
	if got, want := streamedOutput, expectedAnswer; got != want {
		t.Fatalf("%s(userInput=%q): got streamed output %q, want %q", fn, userInput, got, want)
	}

	if got, want := ag.GetStatus(), StatusSettled; got != want {
		t.Fatalf("%s(userInput=%q): got status %v, want %v", fn, userInput, got, want)
	}

	// Verify linear message history sequence
	linear := ag.GetSessionTree().GetLinearContext()
	if got, want := len(linear), 4; got != want {
		t.Fatalf("%s(userInput=%q): got %d messages in linear history, want %d", fn, userInput, got, want)
	}
	if got, want := linear[0].Role, ai.RoleUser; got != want {
		t.Errorf("%s(userInput=%q): msg[0] got role %s, want %s", fn, userInput, got, want)
	}
	if got, want := linear[1].Role, ai.RoleAssistant; got != want || len(linear[1].ToolCalls) != 1 {
		t.Errorf("%s(userInput=%q): msg[1] got role %s tool_calls=%d, want role %s tool_calls=1", fn, userInput, got, len(linear[1].ToolCalls), want)
	}
	if got, want := linear[2].Role, ai.RoleTool; got != want || linear[2].ToolCallID != "call_read_1" {
		t.Errorf("%s(userInput=%q): msg[2] got role %s id %s, want role %s id call_read_1", fn, userInput, got, linear[2].ToolCallID, want)
	}
	if got, want := linear[3].Role, ai.RoleAssistant; got != want || linear[3].Content != expectedAnswer {
		t.Errorf("%s(userInput=%q): msg[3] got role %s content %q, want role %s content %q", fn, userInput, got, linear[3].Content, want, expectedAnswer)
	}
}

func TestAgentLoop_DirectAnswer(t *testing.T) {
	fn := "AgentInstance.Prompt"
	mock := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{
					{DeltaText: "Hello there!"},
				}
			},
		},
	}

	ag := NewAgent("direct-agent", "assistant", mock, "test-model", nil)
	userInput := "hi"
	ans, err := ag.Prompt(context.Background(), userInput)
	if err != nil {
		t.Fatalf("%s(userInput=%q): got unexpected error %v, want nil", fn, userInput, err)
	}
	if got, want := ans, "Hello there!"; got != want {
		t.Fatalf("%s(userInput=%q): got answer %q, want %q", fn, userInput, got, want)
	}
}

func TestAgentLoop_ContextCancel(t *testing.T) {
	fn := "AgentInstance.Prompt"
	mock := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{
					{DeltaText: "partial"},
				}
			},
		},
	}

	ag := NewAgent("cancel-agent", "assistant", mock, "test-model", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	userInput := "hello"
	_, err := ag.Prompt(ctx, userInput)
	if err == nil {
		t.Fatalf("%s(userInput=%q): got nil error on cancelled context, want non-nil error", fn, userInput)
	}
}

// TC-AG-001: ReAct-完整调用闭环多轮收敛
func TestAgentLoop_TC_AG_001_MultiTurnReAct_Convergence(t *testing.T) {
	fn := "AgentInstance.Prompt"
	tmpDir := t.TempDir()
	filePath := setupAgentTestFile(t, tmpDir, "report.txt", "Alpha status: optimal\nBeta status: nominal\n")

	reg, err := builtin.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("builtin.NewDefaultRegistry(): got error %v, want nil", err)
	}

	var observedStatuses []Status
	mock := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			// Turn 0: Model requests `read` on report.txt
			func(req *ai.ChatRequest) []ai.StreamChunk {
				idx := 0
				return []ai.StreamChunk{
					{
						ToolCallDelta: &ai.ToolCall{
							Index: &idx,
							ID:    "call_read_report",
							Type:  "function",
							Function: ai.FunctionCall{
								Name:      "read",
								Arguments: json.RawMessage(fmt.Sprintf(`{"path":%q}`, filePath)),
							},
						},
					},
				}
			},
			// Turn 1: Model reads the tool output and provides final answer
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{
					{DeltaText: "Report summary: "},
					{DeltaText: "Both Alpha and Beta are in optimal or nominal status."},
				}
			},
		},
	}

	ag := NewAgent("react-agent", "researcher", mock, "antigravity/gemini-4-argon", reg)
	ag.SetCwd(tmpDir)

	userInput := "读文件并总结"
	observedStatuses = append(observedStatuses, ag.GetStatus()) // StatusIdle

	ans, err := ag.Prompt(context.Background(), userInput)
	if err != nil {
		t.Fatalf("%s(userInput=%q): got unexpected error %v, want nil", fn, userInput, err)
	}

	wantAnswer := "Report summary: Both Alpha and Beta are in optimal or nominal status."
	if got, want := ans, wantAnswer; got != want {
		t.Errorf("%s(userInput=%q): got answer %q, want %q", fn, userInput, got, want)
	}

	// Verify agent status transitioned to StatusSettled
	if got, want := ag.GetStatus(), StatusSettled; got != want {
		t.Errorf("%s(userInput=%q): got status %v, want %v", fn, userInput, got, want)
	}

	// Verify linear history: User -> Assistant (tool call) -> Tool (result) -> Assistant (summary)
	history := ag.GetSessionTree().GetLinearContext()
	if got, want := len(history), 4; got != want {
		t.Fatalf("%s(userInput=%q): got %d messages in linear history, want %d", fn, userInput, got, want)
	}

	if got, want := history[0].Role, ai.RoleUser; got != want {
		t.Errorf("%s(userInput=%q): msg[0] got role %s, want %s", fn, userInput, got, want)
	}
	if got, want := history[0].Content, userInput; got != want {
		t.Errorf("%s(userInput=%q): msg[0] got content %q, want %q", fn, userInput, got, want)
	}

	if got, want := history[1].Role, ai.RoleAssistant; got != want || len(history[1].ToolCalls) != 1 {
		t.Errorf("%s(userInput=%q): msg[1] got role %s tool_calls=%d, want role %s tool_calls=1", fn, userInput, got, len(history[1].ToolCalls), want)
	}
	if got, want := history[1].ToolCalls[0].ID, "call_read_report"; got != want {
		t.Errorf("%s(userInput=%q): msg[1] tool call ID got %q, want %q", fn, userInput, got, want)
	}

	if got, want := history[2].Role, ai.RoleTool; got != want || history[2].ToolCallID != "call_read_report" {
		t.Errorf("%s(userInput=%q): msg[2] got role %s id %s, want role %s id call_read_report", fn, userInput, got, history[2].ToolCallID, want)
	}
	if !strings.Contains(history[2].Content, "Alpha status: optimal") {
		t.Errorf("%s(userInput=%q): msg[2] tool result %q does not contain expected file content", fn, userInput, history[2].Content)
	}

	if got, want := history[3].Role, ai.RoleAssistant; got != want || history[3].Content != wantAnswer {
		t.Errorf("%s(userInput=%q): msg[3] got role %s content %q, want role %s content %q", fn, userInput, got, history[3].Content, want, wantAnswer)
	}
	_ = observedStatuses
}

type trackingMockTool struct {
	invoked bool
}

func (m *trackingMockTool) Name() string                 { return "mock_tracker" }
func (m *trackingMockTool) Description() string          { return "tracks invocations" }
func (m *trackingMockTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (m *trackingMockTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	m.invoked = true
	return &tools.ToolResult{Content: "invoked"}, nil
}

// TC-AG-002: ReAct-纯文本单轮响应直接收敛
func TestAgentLoop_TC_AG_002_DirectAnswer_Convergence(t *testing.T) {
	fn := "AgentInstance.Prompt"
	trackTool := &trackingMockTool{}
	reg := tools.NewRegistry()
	if err := reg.Register(trackTool); err != nil {
		t.Fatalf("reg.Register(): got error %v, want nil", err)
	}

	mock := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{
					{DeltaText: "你好！我是你的 AI 编程助手。"},
				}
			},
		},
	}

	ag := NewAgent("direct-conv-agent", "assistant", mock, "antigravity/gemini-4-argon", reg)
	userInput := "你好"

	ans, err := ag.Prompt(context.Background(), userInput)
	if err != nil {
		t.Fatalf("%s(userInput=%q): got unexpected error %v, want nil", fn, userInput, err)
	}

	wantAnswer := "你好！我是你的 AI 编程助手。"
	if got, want := ans, wantAnswer; got != want {
		t.Errorf("%s(userInput=%q): got answer %q, want %q", fn, userInput, got, want)
	}

	// Tool should never have been invoked
	if got, want := trackTool.invoked, false; got != want {
		t.Errorf("%s(userInput=%q): got trackTool.invoked=%v, want %v", fn, userInput, got, want)
	}

	// Status direct to settled
	if got, want := ag.GetStatus(), StatusSettled; got != want {
		t.Errorf("%s(userInput=%q): got status %v, want %v", fn, userInput, got, want)
	}

	// Linear context has exactly 2 messages: User and Assistant
	linear := ag.GetSessionTree().GetLinearContext()
	if got, want := len(linear), 2; got != want {
		t.Fatalf("%s(userInput=%q): got %d messages in linear history, want %d", fn, userInput, got, want)
	}
	if got, want := linear[0].Role, ai.RoleUser; got != want {
		t.Errorf("%s(userInput=%q): msg[0] got role %s, want %s", fn, userInput, got, want)
	}
	if got, want := linear[1].Role, ai.RoleAssistant; got != want || linear[1].Content != wantAnswer {
		t.Errorf("%s(userInput=%q): msg[1] got role %s content %q, want role %s content %q", fn, userInput, got, linear[1].Content, want, wantAnswer)
	}
}

// TC-AG-003: ReAct-工具报错反馈模型自愈
func TestAgentLoop_TC_AG_003_ToolError_SelfHealing(t *testing.T) {
	fn := "AgentInstance.Prompt"
	tmpDir := t.TempDir()

	reg, err := builtin.NewDefaultRegistry()
	if err != nil {
		t.Fatalf("builtin.NewDefaultRegistry(): got error %v, want nil", err)
	}

	var turn1ReqMessages []ai.Message
	mock := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			// Turn 0: Model attempts to read a non-existent file
			func(req *ai.ChatRequest) []ai.StreamChunk {
				idx := 0
				return []ai.StreamChunk{
					{
						ToolCallDelta: &ai.ToolCall{
							Index: &idx,
							ID:    "call_read_missing",
							Type:  "function",
							Function: ai.FunctionCall{
								Name:      "read",
								Arguments: json.RawMessage(`{"path":"non_existent_file.txt"}`),
							},
						},
					},
				}
			},
			// Turn 1: Model inspects tool error and self-heals / handles gracefully
			func(req *ai.ChatRequest) []ai.StreamChunk {
				turn1ReqMessages = req.Messages
				return []ai.StreamChunk{
					{DeltaText: "File non_existent_file.txt was not found. Created fallback plan."},
				}
			},
		},
	}

	ag := NewAgent("healing-agent", "assistant", mock, "antigravity/gemini-4-argon", reg)
	ag.SetCwd(tmpDir)

	userInput := "读取 non_existent_file.txt"
	ans, err := ag.Prompt(context.Background(), userInput)
	if err != nil {
		t.Fatalf("%s(userInput=%q): got unexpected error %v, want nil", fn, userInput, err)
	}

	wantAnswer := "File non_existent_file.txt was not found. Created fallback plan."
	if got, want := ans, wantAnswer; got != want {
		t.Errorf("%s(userInput=%q): got answer %q, want %q", fn, userInput, got, want)
	}

	// Verify Turn 1 request contained RoleTool message with error information
	if got, want := len(turn1ReqMessages), 3; got != want {
		t.Fatalf("%s(userInput=%q): Turn 1 received %d messages, want %d", fn, userInput, got, want)
	}
	if got, want := turn1ReqMessages[2].Role, ai.RoleTool; got != want {
		t.Errorf("%s(userInput=%q): Turn 1 msg[2] got role %s, want %s", fn, userInput, got, want)
	}
	if wantSub := "failed to read file"; !strings.Contains(turn1ReqMessages[2].Content, wantSub) {
		t.Errorf("%s(userInput=%q): Turn 1 msg[2] content %q does not contain %q", fn, userInput, turn1ReqMessages[2].Content, wantSub)
	}

	// Verify final linear history has 4 messages: User -> Assistant (tool call) -> Tool (error) -> Assistant (final answer)
	history := ag.GetSessionTree().GetLinearContext()
	if got, want := len(history), 4; got != want {
		t.Fatalf("%s(userInput=%q): got %d history messages, want %d", fn, userInput, got, want)
	}
	if got, want := history[3].Content, wantAnswer; got != want {
		t.Errorf("%s(userInput=%q): final answer got %q, want %q", fn, userInput, got, want)
	}
	if got, want := ag.GetStatus(), StatusSettled; got != want {
		t.Errorf("%s(userInput=%q): got status %v, want %v", fn, userInput, got, want)
	}
}
