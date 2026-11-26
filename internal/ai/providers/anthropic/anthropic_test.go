package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gp/internal/ai"
)

func setupMockServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Provider) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	p := NewProvider("test-key", server.URL)
	p.SetHTTPClient(server.Client())
	return server, p
}

func sseBody(events ...string) string {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("event: message\n")
		b.WriteString("data: ")
		b.WriteString(ev)
		b.WriteString("\n\n")
	}
	return b.String()
}

func drain(t *testing.T, ch <-chan ai.StreamChunk) (text, thinking string, calls map[int]*ai.ToolCall, finish string, usage *ai.TokenUsage) {
	t.Helper()
	calls = make(map[int]*ai.ToolCall)
	var order []int
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("unexpected chunk error: %v", chunk.Error)
		}
		text += chunk.DeltaText
		thinking += chunk.ThinkingDelta
		if tc := chunk.ToolCallDelta; tc != nil {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			acc, ok := calls[idx]
			if !ok {
				acc = &ai.ToolCall{Index: tc.Index}
				calls[idx] = acc
				order = append(order, idx)
			}
			if tc.ID != "" {
				acc.ID = tc.ID
			}
			if tc.Type != "" {
				acc.Type = tc.Type
			}
			acc.Function.Name += tc.Function.Name
			acc.Function.Arguments = append(acc.Function.Arguments, tc.Function.Arguments...)
		}
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	return text, thinking, calls, finish, usage
}

func TestProvider_Name(t *testing.T) {
	p := NewProvider("k", "")
	if got, want := p.Name(), "anthropic"; got != want {
		t.Fatalf("Name(): got %q, want %q", got, want)
	}
}

func TestProvider_MessagesEndpoint(t *testing.T) {
	cases := map[string]string{
		"":                              "https://api.anthropic.com/v1/messages",
		"https://api.anthropic.com":     "https://api.anthropic.com/v1/messages",
		"https://api.anthropic.com/":    "https://api.anthropic.com/v1/messages",
		"https://api.anthropic.com/v1":  "https://api.anthropic.com/v1/messages",
		"https://proxy.local/anthropic": "https://proxy.local/anthropic/v1/messages",
	}
	for base, want := range cases {
		if got := NewProvider("k", base).messagesEndpoint(); got != want {
			t.Errorf("messagesEndpoint(base=%q): got %q, want %q", base, got, want)
		}
	}
}

func TestProvider_StreamChat_Text(t *testing.T) {
	body := sseBody(
		`{"type":"message_start","message":{"usage":{"input_tokens":11,"cache_read_input_tokens":3,"cache_creation_input_tokens":2,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
		`{"type":"message_stop"}`,
	)

	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/v1/messages"; got != want {
			t.Errorf("path: got %q, want %q", got, want)
		}
		if got, want := r.Header.Get("x-api-key"), "test-key"; got != want {
			t.Errorf("x-api-key: got %q, want %q", got, want)
		}
		if got, want := r.Header.Get("anthropic-version"), apiVersion; got != want {
			t.Errorf("anthropic-version: got %q, want %q", got, want)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization: got %q, want empty", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, body)
	})

	ch, err := p.StreamChat(context.Background(), &ai.ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []ai.Message{{Role: ai.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChat(): unexpected error %v", err)
	}

	text, _, _, finish, usage := drain(t, ch)
	if text != "Hello world" {
		t.Fatalf("text: got %q, want %q", text, "Hello world")
	}
	if finish != "stop" {
		t.Fatalf("finish reason: got %q, want %q", finish, "stop")
	}
	if usage == nil {
		t.Fatal("usage: got nil, want non-nil")
	}
	if usage.InputTokens != 16 || usage.OutputTokens != 7 || usage.TotalTokens != 23 {
		t.Fatalf("usage: got %+v, want input=16 output=7 total=23", usage)
	}
	if usage.CacheReadTokens != 3 || usage.CacheWriteTokens != 2 {
		t.Fatalf("cache usage: got read=%d write=%d, want 3/2", usage.CacheReadTokens, usage.CacheWriteTokens)
	}
}

func TestProvider_StreamChat_ToolUse(t *testing.T) {
	body := sseBody(
		`{"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"main.go\"}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	)

	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, body)
	})

	ch, err := p.StreamChat(context.Background(), &ai.ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []ai.Message{{Role: ai.RoleUser, Content: "read main.go"}},
	})
	if err != nil {
		t.Fatalf("StreamChat(): unexpected error %v", err)
	}

	_, _, calls, finish, _ := drain(t, ch)
	call, ok := calls[0]
	if !ok {
		t.Fatalf("tool calls: got %+v, want index 0", calls)
	}
	if call.ID != "toolu_1" || call.Function.Name != "read" {
		t.Fatalf("tool call: got %+v, want id=toolu_1 name=read", call)
	}
	if got, want := string(call.Function.Arguments), `{"path":"main.go"}`; got != want {
		t.Fatalf("tool arguments: got %q, want %q", got, want)
	}
	if finish != "tool_calls" {
		t.Fatalf("finish reason: got %q, want %q", finish, "tool_calls")
	}
}

func TestProvider_StreamChat_ThinkingSignatureReplay(t *testing.T) {
	body := sseBody(
		`{"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"need a tool"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"bash"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	)

	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, body)
	})

	ch, err := p.StreamChat(context.Background(), &ai.ChatRequest{
		Model:         "claude-sonnet-4-5",
		ThinkingLevel: "high",
		Messages:      []ai.Message{{Role: ai.RoleUser, Content: "list files"}},
	})
	if err != nil {
		t.Fatalf("StreamChat(): unexpected error %v", err)
	}

	text, thinking, calls, _, _ := drain(t, ch)
	_ = text
	if thinking != "need a tool" {
		t.Fatalf("thinking: got %q, want %q", thinking, "need a tool")
	}
	toolCall, ok := calls[1]
	if !ok {
		t.Fatalf("tool calls: got %+v, want index 1", calls)
	}
	toolID := toolCall.ID

	// 第二轮：模拟 agent loop 回放助手消息 + 工具结果
	replay := &ai.ChatRequest{
		Model:         "claude-sonnet-4-5",
		ThinkingLevel: "high",
		Messages: []ai.Message{
			{Role: ai.RoleSystem, Content: "be brief"},
			{Role: ai.RoleUser, Content: "list files"},
			{
				Role:     ai.RoleAssistant,
				Content:  "",
				Thinking: thinking,
				ToolCalls: []ai.ToolCall{{
					ID:       toolID,
					Type:     "function",
					Function: ai.FunctionCall{Name: "bash", Arguments: json.RawMessage(`{"cmd":"ls"}`)},
				}},
			},
			{Role: ai.RoleTool, Content: "a.go", ToolCallID: toolID},
		},
	}

	payload, err := json.Marshal(p.buildRequest(replay))
	if err != nil {
		t.Fatalf("marshal replay request: %v", err)
	}
	got := string(payload)

	if !strings.Contains(got, `"signature":"sig-abc"`) {
		t.Fatalf("replay payload missing thinking signature: %s", got)
	}
	if !strings.Contains(got, `"type":"tool_result"`) || !strings.Contains(got, `"tool_use_id":"`+toolID+`"`) {
		t.Fatalf("replay payload missing tool_result block: %s", got)
	}
	if !strings.Contains(got, `"system":"be brief"`) {
		t.Fatalf("replay payload missing hoisted system prompt: %s", got)
	}
	if !strings.Contains(got, `"type":"enabled"`) || !strings.Contains(got, `"budget_tokens":16384`) {
		t.Fatalf("replay payload missing thinking config: %s", got)
	}
	if strings.Contains(got, `"temperature"`) {
		t.Fatalf("replay payload should not carry temperature when thinking enabled: %s", got)
	}
}

func TestProvider_BuildRequest_ToolSchemaAndMerging(t *testing.T) {
	p := NewProvider("k", "")
	req := &ai.ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ai.Message{
			{Role: ai.RoleSystem, Content: "sys-a"},
			{Role: ai.RoleSystem, Content: "sys-b"},
			{Role: ai.RoleUser, Content: "first"},
			{Role: ai.RoleUser, Content: "second"},
			{Role: ai.RoleTool, Content: "r1", ToolCallID: "t1"},
			{Role: ai.RoleTool, Content: "r2", ToolCallID: "t2"},
		},
		Tools: []json.RawMessage{json.RawMessage(`{"type":"function","function":{"name":"read","description":"read file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}`)},
	}

	built := p.buildRequest(req)
	if built.Model != "claude-sonnet-4-5" {
		t.Fatalf("model: got %q", built.Model)
	}
	if built.MaxTokens != defaultMaxTokens {
		t.Fatalf("max_tokens: got %d, want %d", built.MaxTokens, defaultMaxTokens)
	}
	if built.System != "sys-a\n\nsys-b" {
		t.Fatalf("system: got %q, want hoisted system prompt", built.System)
	}
	// Anthropic requires alternating user/assistant turns and places tool_result blocks first within a user turn,
	// so consecutive RoleUser and RoleTool messages merge into a single "user" message with tool_result blocks first.
	if len(built.Messages) != 1 {
		t.Fatalf("messages: got %d, want 1 (consecutive user and tool_result merged into single user turn)", len(built.Messages))
	}
	if built.Messages[0].Role != "user" || len(built.Messages[0].Content) != 4 {
		t.Fatalf("merged turn: got %+v, want 1 user message with 4 blocks", built.Messages[0])
	}
	if built.Messages[0].Content[0]["type"] != "tool_result" || built.Messages[0].Content[1]["type"] != "tool_result" {
		t.Fatalf("expected tool_result blocks placed first, got %+v", built.Messages[0].Content)
	}
	if built.Messages[0].Content[2]["type"] != "text" || built.Messages[0].Content[3]["type"] != "text" {
		t.Fatalf("expected user text blocks after tool_result blocks, got %+v", built.Messages[0].Content)
	}
	if len(built.Tools) != 1 {
		t.Fatalf("tools: got %d, want 1", len(built.Tools))
	}
	if built.Tools[0].Name != "read" || built.Tools[0].Description != "read file" {
		t.Fatalf("tool: got %+v", built.Tools[0])
	}
	if !strings.Contains(string(built.Tools[0].InputSchema), `"path"`) {
		t.Fatalf("tool input_schema: got %s, want parameters passthrough", built.Tools[0].InputSchema)
	}
}

func TestProvider_StreamChat_ErrorStatus(t *testing.T) {
	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`)
	})

	_, err := p.StreamChat(context.Background(), &ai.ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []ai.Message{{Role: ai.RoleUser, Content: "hi"}},
	})
	if err == nil {
		t.Fatal("StreamChat(): got nil error, want non-nil")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("StreamChat(): got error %v, want status and body surfaced", err)
	}
}

func TestProvider_StreamChat_ErrorEvent(t *testing.T) {
	body := sseBody(
		`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"try later"}}`,
	)

	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, body)
	})

	ch, err := p.StreamChat(context.Background(), &ai.ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []ai.Message{{Role: ai.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChat(): unexpected error %v", err)
	}

	var gotErr error
	for chunk := range ch {
		if chunk.Error != nil {
			gotErr = chunk.Error
		}
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "overloaded_error") {
		t.Fatalf("stream error: got %v, want overloaded_error surfaced", gotErr)
	}
}

func TestProvider_StreamChat_RetryWithoutThinkingWhenRejected(t *testing.T) {
	var bodies []string
	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(payload))
		if strings.Contains(string(payload), `"thinking"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"messages.2.content.0.type: Expected thinking or redacted_thinking"}}`)
			return
		}
		_, _ = fmt.Fprint(w, sseBody(
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		))
	})

	ch, err := p.StreamChat(context.Background(), &ai.ChatRequest{
		Model:         "claude-sonnet-4-5",
		ThinkingLevel: "high",
		Messages:      []ai.Message{{Role: ai.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChat(): unexpected error %v", err)
	}
	if _, _, _, finish, _ := drain(t, ch); finish != "stop" {
		t.Fatalf("finish reason: got %q, want stop after retry", finish)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests: got %d, want 2 (thinking then fallback)", len(bodies))
	}
	if !strings.Contains(bodies[0], `"thinking"`) {
		t.Fatalf("first request should enable thinking: %s", bodies[0])
	}
	if strings.Contains(bodies[1], `"thinking"`) {
		t.Fatalf("retry request should drop thinking: %s", bodies[1])
	}
}

func TestProvider_PluginRegistered(t *testing.T) {
	plugin, ok := ai.GetProviderPlugin("claude")
	if !ok {
		t.Fatal("GetProviderPlugin(claude): not registered")
	}
	if plugin.ID != "anthropic" {
		t.Fatalf("plugin id: got %q, want anthropic", plugin.ID)
	}

	prov := ai.CreateProviderFromPlugins("", "sk-ant-api03-xyz", "", "claude-sonnet-4-5")
	if prov == nil || prov.Name() != "anthropic" {
		t.Fatalf("CreateProviderFromPlugins(sk-ant key): got %v, want anthropic provider", prov)
	}
	if got := ai.InferPluginByModel("claude-opus-4-1"); got != "anthropic" {
		t.Fatalf("InferPluginByModel(claude-opus-4-1): got %q, want anthropic", got)
	}
}

func TestProvider_BuildRequest_CacheControlBreakpoints(t *testing.T) {
	p := NewProvider("k", "")
	req := &ai.ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ai.Message{
			{Role: ai.RoleSystem, Content: "stable system", CacheControl: true},
			{Role: ai.RoleUser, Content: "[CONTEXT_LEDGER]\n- rule 1", CacheControl: true},
			{Role: ai.RoleUser, Content: "current query"},
		},
		Tools: []json.RawMessage{
			json.RawMessage(`{"type":"function","function":{"name":"read","description":"read","parameters":{"type":"object"}}}`),
			json.RawMessage(`{"type":"function","function":{"name":"recall_entries","description":"recall","parameters":{"type":"object"}}}`),
		},
	}

	payload, err := json.Marshal(p.buildRequest(req))
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	got := string(payload)
	if !strings.Contains(got, `"system":[{"cache_control":{"type":"ephemeral"},"text":"stable system","type":"text"}]`) {
		t.Fatalf("expected system block with cache_control ephemeral, got: %s", got)
	}
	// Expect 3 cache_control breakpoints: System + last Tool + ContextLedger message
	if count := strings.Count(got, `"cache_control":{"type":"ephemeral"}`); count != 3 {
		t.Fatalf("expected 3 cache_control ephemeral breakpoints (system + tools + ledger), got %d in %s", count, got)
	}
}
