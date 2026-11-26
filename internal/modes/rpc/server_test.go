package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"gp/internal/ai"
	"gp/internal/core/compaction"
	"gp/internal/core/settings"
	"gp/internal/core/storage"
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

func TestServerContextStats_ReadOnlyAndNoLLM(t *testing.T) {
	cfg := settings.DefaultSettings()
	prov := &captureProvider{}
	dbPath := filepath.Join(t.TempDir(), "ctx_stat.db")
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
	send := func(req Request) {
		b, _ := json.Marshal(req)
		_, _ = clientConn.Write(append(b, '\n'))
	}
	readUntil := func(match func(Event) bool) Event {
		for scanner.Scan() {
			var ev Event
			_ = json.Unmarshal(scanner.Bytes(), &ev)
			if ev.Event == EventError {
				t.Fatalf("unexpected RPC error: %+v", ev)
			}
			if match(ev) {
				return ev
			}
		}
		t.Fatalf("stream ended early: err=%v", scanner.Err())
		return Event{}
	}

	// Run 1 turn so session + context_metrics exist
	send(Request{
		ID:        "r1",
		Op:        OpRun,
		SessionID: "chat_stat",
		Prompt:    "hello context stats",
		Metadata:  map[string]string{"chat_id": "oc_999"},
	})
	readUntil(func(ev Event) bool { return ev.Event == EventFinal })

	// Seed a ContextLedger document into the same session in storage
	store := srv.ensureStore()
	ledger := compaction.NewContextLedger()
	ledger.ApplyEvents([]compaction.LedgerEvent{
		{Action: "add", ID: "c1", Category: "constraint", Statement: "Read-only stats command", Provenance: []string{"e1"}},
	})
	rawLedger, _ := json.Marshal(ledger)
	if err := store.Commit(ctx, func(tx storage.Transaction) error {
		return tx.SetDocument("chat_stat", "context_ledger", rawLedger)
	}); err != nil {
		t.Fatalf("SetDocument error = %v", err)
	}

	llmCallsBefore := len(prov.requests)
	send(Request{ID: "s1", Op: OpContextStats, SessionID: "chat_stat"})
	ev := readUntil(func(ev Event) bool { return ev.Event == EventContextStats })

	if len(prov.requests) != llmCallsBefore {
		t.Fatalf("OpContextStats must not invoke LLM! calls before=%d after=%d", llmCallsBefore, len(prov.requests))
	}
	if ev.ContextStats == nil {
		t.Fatalf("expected non-nil ContextStats payload")
	}
	st := ev.ContextStats
	if st.SessionID != "chat_stat" || st.EpochStride != 16 || st.LedgerActiveCnt != 1 || len(st.LedgerHash) != 8 {
		t.Fatalf("unexpected ContextStats header: %+v", st)
	}
	if st.TotalTurns != 1 || len(st.RecentTurns) != 1 {
		t.Fatalf("expected 1 turn metric, got TotalTurns=%d recent=%d", st.TotalTurns, len(st.RecentTurns))
	}
	if len(st.ActiveConstraints) != 1 || st.ActiveConstraints[0].ID != "c1" {
		t.Fatalf("unexpected ActiveConstraints: %+v", st.ActiveConstraints)
	}
}

func TestServerActiveSessionPersistsAcrossRestart(t *testing.T) {
	cfg := settings.DefaultSettings()
	dbPath := filepath.Join(t.TempDir(), "active_session.db")
	homeDir := t.TempDir()
	cwdDir := t.TempDir()

	exchangeOne := func(srv *Server, req Request) Event {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cConn, sConn := net.Pipe()
		defer cConn.Close()
		defer sConn.Close()
		go func() { _ = srv.serveStream(ctx, sConn, sConn) }()

		b, _ := json.Marshal(req)
		_, _ = cConn.Write(append(b, '\n'))

		scanner := bufio.NewScanner(cConn)
		for scanner.Scan() {
			var ev Event
			_ = json.Unmarshal(scanner.Bytes(), &ev)
			if ev.Event == EventError {
				t.Fatalf("unexpected error for %+v: %+v", req.Op, ev)
			}
			if ev.Event == EventFinal || ev.Event == EventResetAck || ev.Event == EventResumeAck || ev.Event == EventContextStats {
				return ev
			}
		}
		t.Fatalf("stream ended unexpectedly for op=%s err=%v", req.Op, scanner.Err())
		return Event{}
	}

	// Server 1: reset (/new) creates chat_p_sess_..., then run one turn
	prov1 := &captureProvider{}
	srv1 := NewServer(RunnerOptions{
		ModelName: "mock/test-model",
		Provider:  prov1,
		Settings:  &cfg,
		DBPath:    dbPath,
		HomeDir:   homeDir,
		Cwd:       cwdDir,
	})
	exchangeOne(srv1, Request{ID: "1", Op: OpResetSession, SessionID: "chat_p"})
	exchangeOne(srv1, Request{ID: "2", Op: OpRun, SessionID: "chat_p", Prompt: "secret after /new"})
	stat1 := exchangeOne(srv1, Request{ID: "3", Op: OpContextStats, SessionID: "chat_p"})
	if stat1.ContextStats == nil || stat1.ContextStats.SessionID == "chat_p" {
		t.Fatalf("expected /new generated session ID, got %+v", stat1.ContextStats)
	}
	generatedID := stat1.ContextStats.SessionID
	_ = srv1.Close()

	// Server 2 (fresh restart, empty in-memory sessionGen): should resolve chat_p -> generatedID from SQLite!
	prov2 := &captureProvider{}
	srv2 := NewServer(RunnerOptions{
		ModelName: "mock/test-model",
		Provider:  prov2,
		Settings:  &cfg,
		DBPath:    dbPath,
		HomeDir:   homeDir,
		Cwd:       cwdDir,
	})
	defer func() { _ = srv2.Close() }()

	stat2 := exchangeOne(srv2, Request{ID: "4", Op: OpContextStats, SessionID: "chat_p"})
	if stat2.ContextStats == nil || stat2.ContextStats.SessionID != generatedID || stat2.ContextStats.TotalTurns != 1 {
		t.Fatalf("expected restarted server to preserve active session %q (1 turn), got %+v", generatedID, stat2.ContextStats)
	}

	// Resume back to original "chat_p" and restart again -> should now resolve to "chat_p"
	exchangeOne(srv2, Request{ID: "5", Op: OpResumeSession, SessionID: "chat_p", TargetID: "chat_p"})
	_ = srv2.Close()

	srv3 := NewServer(RunnerOptions{
		ModelName: "mock/test-model",
		Provider:  &captureProvider{},
		Settings:  &cfg,
		DBPath:    dbPath,
		HomeDir:   homeDir,
		Cwd:       cwdDir,
	})
	defer func() { _ = srv3.Close() }()

	stat3 := exchangeOne(srv3, Request{ID: "6", Op: OpContextStats, SessionID: "chat_p"})
	if stat3.ContextStats == nil || stat3.ContextStats.SessionID != "chat_p" {
		t.Fatalf("expected restarted server after /resume to resolve to 'chat_p', got %+v", stat3.ContextStats)
	}
}
