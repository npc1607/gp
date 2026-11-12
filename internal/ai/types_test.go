package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TC-AI-001: Message兼容reasoning_content
func TestAI_TC_AI_001_MessageReasoningCompatibility(t *testing.T) {
	fn := "Message.UnmarshalJSON"
	inputs := `{"role":"assistant","content":"42","reasoning_content":"thought"}`
	var msg Message
	if err := json.Unmarshal([]byte(inputs), &msg); err != nil {
		t.Fatalf("%s(inputs=%s): got error %v, want nil", fn, inputs, err)
	}

	if got, want := msg.Role, RoleAssistant; got != want {
		t.Fatalf("%s(inputs=%s): Role got %q, want %q", fn, inputs, got, want)
	}
	if got, want := msg.Thinking, "thought"; got != want {
		t.Fatalf("%s(inputs=%s): Thinking got %q, want %q", fn, inputs, got, want)
	}
	if got, want := msg.Content, "42"; got != want {
		t.Fatalf("%s(inputs=%s): Content got %q, want %q", fn, inputs, got, want)
	}
}

// TC-AI-002: Message原生thinking字段优先
func TestAI_TC_AI_002_MessageNativeThinkingPrecedence(t *testing.T) {
	fn := "Message.UnmarshalJSON"
	inputs := `{"role":"assistant","content":"ans","thinking":"t1","reasoning_content":"t2"}`
	var msg Message
	if err := json.Unmarshal([]byte(inputs), &msg); err != nil {
		t.Fatalf("%s(inputs=%s): got error %v, want nil", fn, inputs, err)
	}

	if got, want := msg.Thinking, "t1"; got != want {
		t.Fatalf("%s(inputs=%s): Thinking got %q, want %q (thinking should take priority over reasoning_content)", fn, inputs, got, want)
	}
}

// TC-AI-003: StreamChunk错误接口序列化往返
func TestAI_TC_AI_003_StreamChunkErrorRoundtrip(t *testing.T) {
	fn := "StreamChunk.MarshalJSON / UnmarshalJSON"
	inputs := StreamChunk{
		DeltaText: "delta text",
		Error:     errors.New("stream err"),
	}

	data, err := json.Marshal(inputs)
	if err != nil {
		t.Fatalf("%s(Marshal, inputs=%+v): got error %v, want nil", fn, inputs, err)
	}

	var decoded StreamChunk
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("%s(Unmarshal, data=%s): got error %v, want nil", fn, string(data), err)
	}

	if decoded.Error == nil {
		t.Fatalf("%s(inputs=%+v): decoded.Error got nil, want non-nil", fn, inputs)
	}
	if got, want := decoded.Error.Error(), "stream err"; got != want {
		t.Fatalf("%s(inputs=%+v): decoded.Error.Error() got %q, want %q", fn, inputs, got, want)
	}
	if got, want := decoded.DeltaText, "delta text"; got != want {
		t.Fatalf("%s(inputs=%+v): DeltaText got %q, want %q", fn, inputs, got, want)
	}
}

// TC-AI-004: ToolCall嵌套RawMessage保持
func TestAI_TC_AI_004_ToolCallRawMessagePreservation(t *testing.T) {
	fn := "ToolCall.MarshalJSON / UnmarshalJSON"
	idx := 2
	expectedJSON := `{"path":"internal/core/settings/settings.go","offset":10}`
	inputs := ToolCall{
		Index: &idx,
		ID:    "call_read_123",
		Type:  "function",
		Function: FunctionCall{
			Name:      "read",
			Arguments: json.RawMessage(expectedJSON),
		},
	}

	data, err := json.Marshal(inputs)
	if err != nil {
		t.Fatalf("%s(Marshal, inputs=%+v): got error %v, want nil", fn, inputs, err)
	}

	var decoded ToolCall
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("%s(Unmarshal, data=%s): got error %v, want nil", fn, string(data), err)
	}

	if got, want := decoded.ID, "call_read_123"; got != want {
		t.Fatalf("%s: ID got %q, want %q", fn, got, want)
	}
	if got, want := decoded.Function.Name, "read"; got != want {
		t.Fatalf("%s: Function.Name got %q, want %q", fn, got, want)
	}
	if !bytes.Equal(decoded.Function.Arguments, []byte(expectedJSON)) {
		t.Fatalf("%s: Arguments raw bytes mismatch: got %s, want %s", fn, string(decoded.Function.Arguments), expectedJSON)
	}
}

func TestMessage_JSON(t *testing.T) {
	TestAI_TC_AI_001_MessageReasoningCompatibility(t)
	TestAI_TC_AI_002_MessageNativeThinkingPrecedence(t)

	// Standard full serialization and deserialization
	now := time.Now().Truncate(time.Millisecond)
	original := Message{
		Role:      RoleUser,
		Content:   "Hello world",
		Thinking:  "Thinking process",
		Timestamp: now,
		ToolCalls: []ToolCall{
			{
				ID:   "call_123",
				Type: "function",
				Function: FunctionCall{
					Name:      "read",
					Arguments: json.RawMessage(`{"path":"main.go"}`),
				},
			},
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal Message failed: %v", err)
	}

	var decoded Message
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal Message failed: %v", err)
	}

	if decoded.Role != original.Role || decoded.Content != original.Content || decoded.Thinking != original.Thinking {
		t.Fatalf("Decoded message mismatch: got %+v, want %+v", decoded, original)
	}
	if len(decoded.ToolCalls) != 1 || decoded.ToolCalls[0].ID != "call_123" {
		t.Fatalf("ToolCalls mismatch: got %+v", decoded.ToolCalls)
	}
}

func TestStreamChunk_JSON(t *testing.T) {
	TestAI_TC_AI_003_StreamChunkErrorRoundtrip(t)

	// Full chunk with usage and tool call delta
	idx := 1
	chunk := StreamChunk{
		DeltaText:     "partial text",
		ThinkingDelta: "thinking text",
		ToolCallDelta: &ToolCall{
			Index: &idx,
			ID:    "call_chunk_1",
			Type:  "function",
			Function: FunctionCall{
				Name:      "write",
				Arguments: json.RawMessage(`{"content":"abc"}`),
			},
		},
		FinishReason: "stop",
		Usage: &TokenUsage{
			InputTokens:      10,
			OutputTokens:     20,
			CacheReadTokens:  5,
			CacheWriteTokens: 2,
			TotalTokens:      30,
		},
		Error: errors.New("simulated stream failure"),
	}

	data, err := json.Marshal(chunk)
	if err != nil {
		t.Fatalf("Marshal StreamChunk failed: %v", err)
	}

	var decoded StreamChunk
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal StreamChunk failed: %v", err)
	}

	if decoded.DeltaText != chunk.DeltaText {
		t.Fatalf("DeltaText got %q, want %q", decoded.DeltaText, chunk.DeltaText)
	}
	if decoded.ThinkingDelta != chunk.ThinkingDelta {
		t.Fatalf("ThinkingDelta got %q, want %q", decoded.ThinkingDelta, chunk.ThinkingDelta)
	}
	if decoded.FinishReason != chunk.FinishReason {
		t.Fatalf("FinishReason got %q, want %q", decoded.FinishReason, chunk.FinishReason)
	}
	if decoded.Error == nil || decoded.Error.Error() != "simulated stream failure" {
		t.Fatalf("Error got %v, want %v", decoded.Error, chunk.Error)
	}
	if decoded.Usage == nil || decoded.Usage.TotalTokens != 30 {
		t.Fatalf("Usage got %+v, want TotalTokens=30", decoded.Usage)
	}
	if decoded.ToolCallDelta == nil || decoded.ToolCallDelta.ID != "call_chunk_1" {
		t.Fatalf("ToolCallDelta got %+v, want ID=call_chunk_1", decoded.ToolCallDelta)
	}

	// Clean chunk with nil error
	cleanChunk := StreamChunk{
		DeltaText: "hello",
	}
	cleanData, err := json.Marshal(cleanChunk)
	if err != nil {
		t.Fatalf("Marshal clean StreamChunk failed: %v", err)
	}
	var cleanDecoded StreamChunk
	if err := json.Unmarshal(cleanData, &cleanDecoded); err != nil {
		t.Fatalf("Unmarshal clean StreamChunk failed: %v", err)
	}
	if cleanDecoded.Error != nil {
		t.Fatalf("Expected nil error, got %v", cleanDecoded.Error)
	}
}

func TestToolCall_JSON(t *testing.T) {
	TestAI_TC_AI_004_ToolCallRawMessagePreservation(t)
}

func TestTokenUsage_JSON(t *testing.T) {
	fn := "TokenUsage.MarshalJSON / UnmarshalJSON"
	inputs := TokenUsage{
		InputTokens:      100,
		OutputTokens:     50,
		CacheReadTokens:  20,
		CacheWriteTokens: 10,
		TotalTokens:      150,
	}

	data, err := json.Marshal(inputs)
	if err != nil {
		t.Fatalf("%s(Marshal, inputs=%+v): got error %v", fn, inputs, err)
	}

	var decoded TokenUsage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("%s(Unmarshal, data=%s): got error %v", fn, string(data), err)
	}

	if decoded != inputs {
		t.Fatalf("%s: Decoded TokenUsage got %+v, want %+v", fn, decoded, inputs)
	}
}

func TestConstructors(t *testing.T) {
	fn := "Constructors"
	before := time.Now().Add(-time.Second)

	// 1. NewUserMessage
	userMsg := NewUserMessage("question")
	if got, want := userMsg.Role, RoleUser; got != want {
		t.Fatalf("%s(NewUserMessage): Role got %q, want %q", fn, got, want)
	}
	if got, want := userMsg.Content, "question"; got != want {
		t.Fatalf("%s(NewUserMessage): Content got %q, want %q", fn, got, want)
	}
	if userMsg.Timestamp.Before(before) {
		t.Fatalf("%s(NewUserMessage): Timestamp not set properly: %v", fn, userMsg.Timestamp)
	}

	// 2. NewAssistantMessage
	asstMsg := NewAssistantMessage("answer", "reasoning")
	if got, want := asstMsg.Role, RoleAssistant; got != want {
		t.Fatalf("%s(NewAssistantMessage): Role got %q, want %q", fn, got, want)
	}
	if got, want := asstMsg.Content, "answer"; got != want {
		t.Fatalf("%s(NewAssistantMessage): Content got %q, want %q", fn, got, want)
	}
	if got, want := asstMsg.Thinking, "reasoning"; got != want {
		t.Fatalf("%s(NewAssistantMessage): Thinking got %q, want %q", fn, got, want)
	}

	// 3. NewSystemMessage
	sysMsg := NewSystemMessage("system prompt")
	if got, want := sysMsg.Role, RoleSystem; got != want {
		t.Fatalf("%s(NewSystemMessage): Role got %q, want %q", fn, got, want)
	}
	if got, want := sysMsg.Content, "system prompt"; got != want {
		t.Fatalf("%s(NewSystemMessage): Content got %q, want %q", fn, got, want)
	}

	// 4. NewToolMessage
	toolMsg := NewToolMessage("call_999", "tool output result")
	if got, want := toolMsg.Role, RoleTool; got != want {
		t.Fatalf("%s(NewToolMessage): Role got %q, want %q", fn, got, want)
	}
	if got, want := toolMsg.ToolCallID, "call_999"; got != want {
		t.Fatalf("%s(NewToolMessage): ToolCallID got %q, want %q", fn, got, want)
	}
	if got, want := toolMsg.Content, "tool output result"; got != want {
		t.Fatalf("%s(NewToolMessage): Content got %q, want %q", fn, got, want)
	}
}
