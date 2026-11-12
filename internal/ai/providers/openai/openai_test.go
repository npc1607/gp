package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gp/internal/ai"
)

func setupMockServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Provider) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		server.Close()
	})
	p := NewProvider("test-key", server.URL)
	p.SetHTTPClient(server.Client())
	return server, p
}

func TestOpenAIProvider_Name(t *testing.T) {
	fn := "Provider.Name"
	p := NewProvider("dummy-key", "https://api.openai.com/v1")
	if got, want := p.Name(), "openai"; got != want {
		t.Fatalf("%s(): got %q, want %q", fn, got, want)
	}
}

func TestOpenAIProvider_StreamChat_Text(t *testing.T) {
	fn := "Provider.StreamChat"
	sseData := `data: {"id":"chat-1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}

data: {"id":"chat-1","choices":[{"index":0,"delta":{"content":" world!"},"finish_reason":"stop"}]}

data: [DONE]
`
	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer test-key"; got != want {
			t.Errorf("HTTP Header Authorization: got %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, sseData)
	})

	ctx := context.Background()
	req := &ai.ChatRequest{
		Model: "gpt-4o",
		Messages: []ai.Message{
			{Role: ai.RoleUser, Content: "hi"},
		},
	}

	ch, err := p.StreamChat(ctx, req)
	if err != nil {
		t.Fatalf("%s(req=%+v): got unexpected error %v, want nil", fn, req, err)
	}

	var fullText string
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("%s(req=%+v): got chunk error %v, want nil", fn, req, chunk.Error)
		}
		fullText += chunk.DeltaText
	}

	expected := "Hello world!"
	if got, want := fullText, expected; got != want {
		t.Fatalf("%s(req=%+v): got fullText %q, want %q", fn, req, got, want)
	}
}

func TestOpenAIProvider_StreamChat_Reasoning(t *testing.T) {
	fn := "Provider.StreamChat"
	sseData := `data: {"id":"chat-2","choices":[{"index":0,"delta":{"reasoning_content":"Let's think..."}}]}

data: {"id":"chat-2","choices":[{"index":0,"delta":{"content":"Answer: 42"}}]}

data: [DONE]
`
	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, sseData)
	})

	ctx := context.Background()
	req := &ai.ChatRequest{
		Model: "deepseek-r1",
		Messages: []ai.Message{
			{Role: ai.RoleUser, Content: "what is 6 * 7?"},
		},
	}

	ch, err := p.StreamChat(ctx, req)
	if err != nil {
		t.Fatalf("%s(req=%+v): got unexpected error %v, want nil", fn, req, err)
	}

	var thinking, answer string
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("%s(req=%+v): got chunk error %v, want nil", fn, req, chunk.Error)
		}
		thinking += chunk.ThinkingDelta
		answer += chunk.DeltaText
	}

	if got, want := thinking, "Let's think..."; got != want {
		t.Fatalf("%s(req=%+v): got thinking %q, want %q", fn, req, got, want)
	}
	if got, want := answer, "Answer: 42"; got != want {
		t.Fatalf("%s(req=%+v): got answer %q, want %q", fn, req, got, want)
	}
}

func TestOpenAIProvider_StreamChat_ToolCalls(t *testing.T) {
	fn := "Provider.StreamChat"
	sseData := `data: {"id":"chat-3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_123","type":"function","function":{"name":"read","arguments":""}}]}}]}

data: {"id":"chat-3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"foo.txt\"}"}}]}}]}

data: [DONE]
`
	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, sseData)
	})

	ctx := context.Background()
	req := &ai.ChatRequest{
		Model: "gpt-4o",
		Messages: []ai.Message{
			{Role: ai.RoleUser, Content: "read foo.txt"},
		},
	}

	ch, err := p.StreamChat(ctx, req)
	if err != nil {
		t.Fatalf("%s(req=%+v): got unexpected error %v, want nil", fn, req, err)
	}

	var callID, funcName, args string
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("%s(req=%+v): got chunk error %v, want nil", fn, req, chunk.Error)
		}
		if chunk.ToolCallDelta != nil {
			tc := chunk.ToolCallDelta
			if tc.ID != "" {
				callID = tc.ID
			}
			if tc.Function.Name != "" {
				funcName = tc.Function.Name
			}
			args += string(tc.Function.Arguments)
		}
	}

	if got, want := callID, "call_123"; got != want {
		t.Fatalf("%s(req=%+v): got callID %q, want %q", fn, req, got, want)
	}
	if got, want := funcName, "read"; got != want {
		t.Fatalf("%s(req=%+v): got funcName %q, want %q", fn, req, got, want)
	}
	if got, want := args, `{"path":"foo.txt"}`; got != want {
		t.Fatalf("%s(req=%+v): got args %q, want %q", fn, req, got, want)
	}
}

func TestOpenAIProvider_StreamChat_TokenUsage(t *testing.T) {
	fn := "Provider.StreamChat"
	sseData := `data: {"id":"chat-4","choices":[{"index":0,"delta":{"content":"Hi"}}]}

data: {"id":"chat-4","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}

data: [DONE]
`
	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, sseData)
	})

	ctx := context.Background()
	req := &ai.ChatRequest{
		Model: "gpt-4o",
		Messages: []ai.Message{
			{Role: ai.RoleUser, Content: "hi"},
		},
	}

	ch, err := p.StreamChat(ctx, req)
	if err != nil {
		t.Fatalf("%s(req=%+v): got unexpected error %v, want nil", fn, req, err)
	}

	var finalUsage *ai.TokenUsage
	for chunk := range ch {
		if chunk.Usage != nil {
			finalUsage = chunk.Usage
		}
	}

	if finalUsage == nil {
		t.Fatalf("%s(req=%+v): got nil finalUsage, want non-nil", fn, req)
	}
	if got, want := finalUsage.InputTokens, int64(10); got != want {
		t.Errorf("%s(req=%+v): got InputTokens=%d, want %d", fn, req, got, want)
	}
	if got, want := finalUsage.OutputTokens, int64(5); got != want {
		t.Errorf("%s(req=%+v): got OutputTokens=%d, want %d", fn, req, got, want)
	}
	if got, want := finalUsage.TotalTokens, int64(15); got != want {
		t.Errorf("%s(req=%+v): got TotalTokens=%d, want %d", fn, req, got, want)
	}
	if got, want := finalUsage.CacheReadTokens, int64(4); got != want {
		t.Errorf("%s(req=%+v): got CacheReadTokens=%d, want %d", fn, req, got, want)
	}
}

func TestOpenAIProvider_StreamChat_ErrorStatus(t *testing.T) {
	fn := "Provider.StreamChat"
	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"Invalid API key"}}`)
	})

	ctx := context.Background()
	req := &ai.ChatRequest{
		Model: "gpt-4o",
		Messages: []ai.Message{
			{Role: ai.RoleUser, Content: "hello"},
		},
	}

	ch, err := p.StreamChat(ctx, req)
	if err == nil {
		t.Fatalf("%s(req=%+v): got error nil, want non-nil error", fn, req)
	}
	if ch != nil {
		t.Fatalf("%s(req=%+v): got channel non-nil, want nil", fn, req)
	}
}

func TestOpenAIProvider_StreamChat_ContextCancel(t *testing.T) {
	fn := "Provider.StreamChat"
	hang := make(chan struct{})
	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-hang
	})
	t.Cleanup(func() {
		select {
		case <-hang:
		default:
			close(hang)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := &ai.ChatRequest{
		Model: "gpt-4o",
		Messages: []ai.Message{
			{Role: ai.RoleUser, Content: "hello"},
		},
	}

	ch, err := p.StreamChat(ctx, req)
	if err != nil {
		t.Fatalf("%s(req=%+v): got unexpected error %v, want nil", fn, req, err)
	}

	time.AfterFunc(20*time.Millisecond, cancel)

	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatalf("%s(req=%+v): stream did not abort on context cancellation within 2s, want immediate abort", fn, req)
	}
}

// TC-AI-001: OpenAI-SSE流式文本与Thinking解析
func TestOpenAIProvider_TC_AI_001_SSE_Text_And_Thinking(t *testing.T) {
	fn := "Provider.StreamChat"
	sseData := `data: {"id":"chat-ai-001","choices":[{"index":0,"delta":{"reasoning_content":"Thinking about question..."}}]}

data: {"id":"chat-ai-001","choices":[{"index":0,"delta":{"content":"Resolved "}}]}

data: {"id":"chat-ai-001","choices":[{"index":0,"delta":{"content":"answer."}}]}

data: {"id":"chat-ai-001","choices":[],"usage":{"prompt_tokens":25,"completion_tokens":12,"total_tokens":37,"prompt_tokens_details":{"cached_tokens":8}}}

data: [DONE]
`
	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, sseData)
	})

	ctx := context.Background()
	req := &ai.ChatRequest{
		Model: "deepseek-reasoner",
		Messages: []ai.Message{
			{Role: ai.RoleUser, Content: "Solve problem"},
		},
	}

	ch, err := p.StreamChat(ctx, req)
	if err != nil {
		t.Fatalf("%s(req=%+v): got unexpected error %v, want nil", fn, req, err)
	}

	var thinking, fullText string
	var finalUsage *ai.TokenUsage

	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("%s(req=%+v): got chunk error %v, want nil", fn, req, chunk.Error)
		}
		thinking += chunk.ThinkingDelta
		fullText += chunk.DeltaText
		if chunk.Usage != nil {
			finalUsage = chunk.Usage
		}
	}

	if got, want := thinking, "Thinking about question..."; got != want {
		t.Errorf("%s(req=%+v): got thinking %q, want %q", fn, req, got, want)
	}
	if got, want := fullText, "Resolved answer."; got != want {
		t.Errorf("%s(req=%+v): got text %q, want %q", fn, req, got, want)
	}
	if finalUsage == nil {
		t.Fatalf("%s(req=%+v): got nil finalUsage, want non-nil", fn, req)
	}
	if got, want := finalUsage.InputTokens, int64(25); got != want {
		t.Errorf("%s(req=%+v): got InputTokens=%d, want %d", fn, req, got, want)
	}
	if got, want := finalUsage.OutputTokens, int64(12); got != want {
		t.Errorf("%s(req=%+v): got OutputTokens=%d, want %d", fn, req, got, want)
	}
	if got, want := finalUsage.TotalTokens, int64(37); got != want {
		t.Errorf("%s(req=%+v): got TotalTokens=%d, want %d", fn, req, got, want)
	}
	if got, want := finalUsage.CacheReadTokens, int64(8); got != want {
		t.Errorf("%s(req=%+v): got CacheReadTokens=%d, want %d", fn, req, got, want)
	}
}

// TC-AI-002: OpenAI-多Chunk分片工具调用拼装
func TestOpenAIProvider_TC_AI_002_ToolCalls_MultiChunk(t *testing.T) {
	fn := "Provider.StreamChat"
	// SSE sends 3 chunks for a single tool call:
	// Chunk 1: index, id, type, function name, partial arguments
	// Chunk 2: partial arguments
	// Chunk 3: partial arguments completing the JSON
	sseData := `data: {"id":"tc-002","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_write_abc","type":"function","function":{"name":"write","arguments":"{\"path\":\"output.txt\","}}]}}]}

data: {"id":"tc-002","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"content\":"}}]}}]}

data: {"id":"tc-002","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"hello world\"}"}}]}}]}

data: [DONE]
`
	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, sseData)
	})

	ctx := context.Background()
	req := &ai.ChatRequest{
		Model: "gpt-4o",
		Messages: []ai.Message{
			{Role: ai.RoleUser, Content: "write to output.txt"},
		},
	}

	ch, err := p.StreamChat(ctx, req)
	if err != nil {
		t.Fatalf("%s(req=%+v): got unexpected error %v, want nil", fn, req, err)
	}

	var callID, funcName string
	var argsBuilder strings.Builder
	chunkCount := 0

	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("%s(req=%+v): got chunk error %v, want nil", fn, req, chunk.Error)
		}
		if chunk.ToolCallDelta != nil {
			chunkCount++
			tc := chunk.ToolCallDelta
			if tc.ID != "" {
				callID = tc.ID
			}
			if tc.Function.Name != "" {
				funcName = tc.Function.Name
			}
			if len(tc.Function.Arguments) > 0 {
				argsBuilder.Write(tc.Function.Arguments)
			}
		}
	}

	if got, want := chunkCount, 3; got != want {
		t.Errorf("%s(req=%+v): got %d tool call chunks, want %d", fn, req, got, want)
	}
	if got, want := callID, "call_write_abc"; got != want {
		t.Errorf("%s(req=%+v): got callID %q, want %q", fn, req, got, want)
	}
	if got, want := funcName, "write"; got != want {
		t.Errorf("%s(req=%+v): got funcName %q, want %q", fn, req, got, want)
	}

	assembledArgs := argsBuilder.String()
	wantArgs := `{"path":"output.txt","content":"hello world"}`
	if got, want := assembledArgs, wantArgs; got != want {
		t.Errorf("%s(req=%+v): got raw args %q, want %q", fn, req, got, want)
	}

	// Verify lossless JSON unmarshal without panic
	var parsedArgs map[string]string
	if unmarshalErr := json.Unmarshal([]byte(assembledArgs), &parsedArgs); unmarshalErr != nil {
		t.Fatalf("%s(req=%+v): JSON unmarshal failed %v, want valid JSON (raw=%q)", fn, req, unmarshalErr, assembledArgs)
	}
	if got, want := parsedArgs["path"], "output.txt"; got != want {
		t.Errorf("%s(req=%+v): got parsed path %q, want %q", fn, req, got, want)
	}
	if got, want := parsedArgs["content"], "hello world"; got != want {
		t.Errorf("%s(req=%+v): got parsed content %q, want %q", fn, req, got, want)
	}
}

// TC-AI-003: OpenAI-Context超时主动中止与释放
func TestOpenAIProvider_TC_AI_003_ContextCancel(t *testing.T) {
	fn := "Provider.StreamChat"
	hang := make(chan struct{})
	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-hang
	})
	t.Cleanup(func() {
		select {
		case <-hang:
		default:
			close(hang)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	req := &ai.ChatRequest{
		Model: "gpt-4o",
		Messages: []ai.Message{
			{Role: ai.RoleUser, Content: "hang test"},
		},
	}

	ch, err := p.StreamChat(ctx, req)
	if err != nil {
		t.Fatalf("%s(req=%+v): got unexpected error %v, want nil", fn, req, err)
	}

	// Cancel context midway
	time.AfterFunc(20*time.Millisecond, cancel)

	closedCleanly := make(chan struct{})
	go func() {
		for range ch {
		}
		close(closedCleanly)
	}()

	select {
	case <-closedCleanly:
		// Passed: stream closed cleanly
	case <-time.After(2 * time.Second):
		t.Fatalf("%s(req=%+v): stream did not terminate cleanly on context cancellation, want channel closed", fn, req)
	}
}

// TC-AI-004: OpenAI-HTTP非200状态码响应拦截
func TestOpenAIProvider_TC_AI_004_ErrorStatus401(t *testing.T) {
	fn := "Provider.StreamChat"
	_, p := setupMockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"message":"Unauthorized: Invalid API key provided"}}`)
	})

	ctx := context.Background()
	req := &ai.ChatRequest{
		Model: "gpt-4o",
		Messages: []ai.Message{
			{Role: ai.RoleUser, Content: "unauthorized test"},
		},
	}

	ch, err := p.StreamChat(ctx, req)
	if err == nil {
		t.Fatalf("%s(req=%+v): got nil error, want error with 401 status", fn, req)
	}
	if ch != nil {
		t.Fatalf("%s(req=%+v): got non-nil channel, want nil", fn, req)
	}

	errStr := err.Error()
	if !strings.Contains(errStr, "401") {
		t.Errorf("%s(req=%+v): got error %q, want containing '401'", fn, req, errStr)
	}
	if !strings.Contains(errStr, "Invalid API key") {
		t.Errorf("%s(req=%+v): got error %q, want containing 'Invalid API key'", fn, req, errStr)
	}
}
