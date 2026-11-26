package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"gp/internal/ai"
	"gp/internal/core/settings"
)

// slowProvider 模拟一个流式输出耗时约 1.5s 的模型，用于测试运行中断与工具调用事件去重。
type slowProvider struct {
	chunks int
	delay  time.Duration
}

func (m *slowProvider) Name() string { return "slow" }
func (m *slowProvider) StreamChat(ctx context.Context, req *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	ch := make(chan ai.StreamChunk, m.chunks+2)
	go func() {
		defer close(ch)
		if req != nil {
			hasToolResult := false
			for _, msg := range req.Messages {
				if msg.Role == ai.RoleSystem && strings.Contains(msg.Content, "安全审计子 Agent") {
					ch <- ai.StreamChunk{DeltaText: `{"risk_level":"low","allow":true,"can_approve":false,"reason":"probe allow"}`}
					ch <- ai.StreamChunk{FinishReason: "stop"}
					return
				}
				if msg.Role == ai.RoleTool {
					hasToolResult = true
				}
			}
			if hasToolResult {
				ch <- ai.StreamChunk{DeltaText: "done"}
				ch <- ai.StreamChunk{FinishReason: "stop"}
				return
			}
		}
		for i := 0; i < m.chunks; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(m.delay):
			}
			// 每个分片都带同一个工具名，模拟分片重复上报
			ch <- ai.StreamChunk{
				DeltaText:     "x",
				ToolCallDelta: &ai.ToolCall{Type: "function", Function: ai.FunctionCall{Name: "bash"}},
			}
		}
		ch <- ai.StreamChunk{FinishReason: "stop"}
	}()
	return ch, nil
}

func probeServer(t *testing.T, p ai.Provider) (net.Conn, func()) {
	cfg := settings.DefaultSettings()
	srv := NewServer(RunnerOptions{
		ModelName:    "mock/x",
		Provider:     p,
		Settings:     &cfg,
		HomeDir:      t.TempDir(),
		Cwd:          t.TempDir(),
		AgentVersion: "probe",
	})

	// 真实 TCP 监听，避免 net.Pipe 无缓冲导致的双向阻塞
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { _ = srv.serveStream(ctx, c, c) }(c)
		}
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return client, func() { cancel(); client.Close(); ln.Close() }
}

// 探针 1：运行期间发送 abort，看是否能在运行结束前拿到 abort_ack
func TestProbe_AbortDuringRun(t *testing.T) {
	client, cleanup := probeServer(t, &slowProvider{chunks: 6, delay: 250 * time.Millisecond})
	defer cleanup()

	enc := json.NewEncoder(client)
	_ = enc.Encode(Request{ID: "1", Op: OpRun, Prompt: "hello"})

	// 等运行真正开始（收到 started）
	sc := bufio.NewScanner(client)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	start := time.Now()
	sawStarted := false
	for sc.Scan() {
		var ev Event
		_ = json.Unmarshal(sc.Bytes(), &ev)
		if ev.Event == EventStarted {
			sawStarted = true
			break
		}
	}
	if !sawStarted {
		t.Fatalf("未收到 started 事件")
	}

	// 运行中发送 abort
	_ = enc.Encode(Request{ID: "9", Op: OpAbort, TargetID: "1"})

	// 期望：abort_ack 在很短时间内到达，且不会再收到 final
	deadline := time.After(3 * time.Second)
	gotAck := false
	gotFinal := false
	for {
		select {
		case <-deadline:
			t.Fatalf("超时：ack=%v final=%v", gotAck, gotFinal)
		default:
		}
		if !sc.Scan() {
			break
		}
		var ev Event
		_ = json.Unmarshal(sc.Bytes(), &ev)
		if ev.Event == EventAbortAck {
			gotAck = true
			t.Logf("✓ 收到 abort_ack，耗时 %v", time.Since(start).Round(10*time.Millisecond))
			break
		}
		if ev.Event == EventFinal {
			gotFinal = true
			t.Logf("✗ 在 abort_ack 之前先收到了 final，耗时 %v", time.Since(start).Round(10*time.Millisecond))
			break
		}
	}
	if !gotAck {
		t.Errorf("✗ 运行中 abort 无效：先收到 final=%v，abort 无法中断", gotFinal)
	}
}

// 探针 2：同一工具调用的多个流式分片是否会产生重复 tool_start
func TestProbe_DuplicateToolStart(t *testing.T) {
	client, cleanup := probeServer(t, &slowProvider{chunks: 4, delay: 5 * time.Millisecond})
	defer cleanup()

	enc := json.NewEncoder(client)
	_ = enc.Encode(Request{ID: "1", Op: OpRun, Prompt: "hello"})

	sc := bufio.NewScanner(client)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	toolStarts, toolEnds := 0, 0
	for sc.Scan() {
		var ev Event
		_ = json.Unmarshal(sc.Bytes(), &ev)
		switch ev.Event {
		case EventToolStart:
			toolStarts++
		case EventToolEnd:
			toolEnds++
		case EventFinal:
			t.Logf("tool_start=%d tool_end=%d（真实工具调用应各为 1）", toolStarts, toolEnds)
			if toolStarts != 1 {
				t.Errorf("✗ tool_start 重复上报 %d 次", toolStarts)
			}
			return
		}
	}
}
