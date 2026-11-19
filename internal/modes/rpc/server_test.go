package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"gp/internal/ai"
	"gp/internal/core/settings"
)

type mockProvider struct {
	reply string
}

func (m *mockProvider) Name() string { return "mock" }
func (m *mockProvider) StreamChat(_ context.Context, _ *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	ch := make(chan ai.StreamChunk, 3)
	go func() {
		defer close(ch)
		ch <- ai.StreamChunk{DeltaText: m.reply}
		ch <- ai.StreamChunk{FinishReason: "stop"}
	}()
	return ch, nil
}

func TestServerAGPStream(t *testing.T) {
	cfg := settings.DefaultSettings()
	srv := NewServer(RunnerOptions{
		ModelName:    "mock/test-model",
		Provider:     &mockProvider{reply: "你好，来自 GP 的回复"},
		Settings:     &cfg,
		HomeDir:      t.TempDir(),
		Cwd:          t.TempDir(),
		AgentVersion: "test-v1",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	go func() {
		_ = srv.serveStream(ctx, serverConn, serverConn)
	}()

	sendReq := func(req Request) {
		b, _ := json.Marshal(req)
		b = append(b, '\n')
		if _, err := clientConn.Write(b); err != nil {
			t.Fatalf("write request: %v", err)
		}
	}

	scanner := bufio.NewScanner(clientConn)
	readEvent := func() Event {
		if !scanner.Scan() {
			t.Fatalf("expected event line, err=%v", scanner.Err())
		}
		var ev Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatalf("unmarshal event: %v", err)
		}
		return ev
	}

	// 1. Hello
	sendReq(Request{ID: "1", Op: OpHello, Client: "test"})
	ev := readEvent()
	if ev.Event != EventHelloAck || ev.AgentName != "gp" {
		t.Fatalf("unexpected hello_ack: %+v", ev)
	}

	// 2. Run
	sendReq(Request{ID: "2", Op: OpRun, SessionID: "chat_1", Prompt: "hello"})
	var gotFinal string
	var gotDelta string
	for {
		ev = readEvent()
		if ev.Event == EventError {
			t.Fatalf("unexpected error event: %+v", ev)
		}
		if ev.Event == EventDelta {
			gotDelta += ev.Text
		}
		if ev.Event == EventFinal {
			gotFinal = ev.Text
			break
		}
	}
	if gotFinal != "你好，来自 GP 的回复" || gotDelta != "你好，来自 GP 的回复" {
		t.Fatalf("unexpected text: final=%q delta=%q", gotFinal, gotDelta)
	}

	// 3. ResetSession
	sendReq(Request{ID: "3", Op: OpResetSession, SessionID: "chat_1"})
	ev = readEvent()
	if ev.Event != EventResetAck {
		t.Fatalf("unexpected reset_ack: %+v", ev)
	}
}

type captureProvider struct {
	requests []*ai.ChatRequest
}

func (c *captureProvider) Name() string { return "capture" }
func (c *captureProvider) StreamChat(_ context.Context, req *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	c.requests = append(c.requests, req)
	ch := make(chan ai.StreamChunk, 2)
	go func() {
		defer close(ch)
		ch <- ai.StreamChunk{DeltaText: "ok"}
		ch <- ai.StreamChunk{FinishReason: "stop"}
	}()
	return ch, nil
}

func TestServerAGPMultiTurnMemory(t *testing.T) {
	cfg := settings.DefaultSettings()
	prov := &captureProvider{}
	dbPath := filepath.Join(t.TempDir(), "test.db")
	srv := NewServer(RunnerOptions{
		ModelName: "mock/test-model",
		Provider:  prov,
		Settings:  &cfg,
		DBPath:    dbPath,
		HomeDir:   t.TempDir(),
		Cwd:       t.TempDir(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	go func() { _ = srv.serveStream(ctx, serverConn, serverConn) }()

	scanner := bufio.NewScanner(clientConn)
	runTurn := func(id, sessID, prompt string) {
		b, _ := json.Marshal(Request{ID: id, Op: OpRun, SessionID: sessID, Prompt: prompt})
		_, _ = clientConn.Write(append(b, '\n'))
		for scanner.Scan() {
			var ev Event
			_ = json.Unmarshal(scanner.Bytes(), &ev)
			if ev.Event == EventFinal {
				return
			}
			if ev.Event == EventError {
				t.Fatalf("turn %s error: %+v", id, ev)
			}
		}
	}

	runTurn("1", "group_1", "记住我的名字叫张三")
	runTurn("2", "group_2", "我是李四")
	runTurn("3", "group_1", "我叫什么名字？")
	runTurn("4", "group_2", "我是谁？")

	if len(prov.requests) < 4 {
		t.Fatalf("expected 4 requests, got %d", len(prov.requests))
	}
	turn3Msgs := prov.requests[2].Messages
	t.Logf("group_1 turn 2 message count: %d", len(turn3Msgs))
	foundG1 := false
	for _, m := range turn3Msgs {
		if m.Content == "记住我的名字叫张三" {
			foundG1 = true
			break
		}
	}
	if !foundG1 {
		t.Fatalf("group_1 turn 2 did NOT contain turn 1 message! messages=%+v", turn3Msgs)
	}
	turn4Msgs := prov.requests[3].Messages
	foundG2 := false
	for _, m := range turn4Msgs {
		if m.Content == "我是李四" {
			foundG2 = true
			break
		}
	}
	if !foundG2 {
		t.Fatalf("group_2 turn 2 did NOT contain turn 1 message! messages=%+v", turn4Msgs)
	}
}
