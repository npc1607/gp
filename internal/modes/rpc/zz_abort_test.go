package rpc

import (
	"bufio"
	"context"
	"encoding/json"
	"testing"
	"time"

	"gp/internal/ai"
)

// textProvider 只输出文本，分 8 片、每片 250ms，总耗时约 2s，且会响应 ctx 取消。
type textProvider struct{}

func (p *textProvider) Name() string { return "text" }
func (p *textProvider) StreamChat(ctx context.Context, _ *ai.ChatRequest) (<-chan ai.StreamChunk, error) {
	ch := make(chan ai.StreamChunk, 16)
	go func() {
		defer close(ch)
		for i := 0; i < 8; i++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
			}
			ch <- ai.StreamChunk{DeltaText: "x"}
		}
		ch <- ai.StreamChunk{FinishReason: "stop"}
	}()
	return ch, nil
}

func TestProbe_AbortMidRun(t *testing.T) {
	client, cleanup := probeServer(t, &textProvider{})
	defer cleanup()

	start := time.Now()
	enc := json.NewEncoder(client)
	_ = enc.Encode(Request{ID: "run-1", Op: OpRun, Prompt: "hello"})

	sc := bufio.NewScanner(client)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)

	events := make(chan Event, 256)
	go func() {
		defer close(events)
		for sc.Scan() {
			var ev Event
			if json.Unmarshal(sc.Bytes(), &ev) == nil {
				events <- ev
			}
		}
	}()

	// 等 started
	for ev := range events {
		if ev.Event == EventStarted {
			t.Logf("%6v started", time.Since(start).Round(10*time.Millisecond))
			break
		}
	}

	// 运行中发 abort
	time.Sleep(300 * time.Millisecond)
	t.Logf("%6v → 发送 abort(target=run-1)", time.Since(start).Round(10*time.Millisecond))
	_ = enc.Encode(Request{ID: "abort-1", Op: OpAbort, TargetID: "run-1"})

	timeout := time.After(8 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("连接关闭但既无 abort_ack 也无 final")
			}
			switch ev.Event {
			case EventAbortAck:
				t.Logf("%6v ✓ abort_ack 到达", time.Since(start).Round(10*time.Millisecond))
			case EventFinal:
				t.Logf("%6v final 到达：abort_ack 是否更早？", time.Since(start).Round(10*time.Millisecond))
			case EventError:
				t.Logf("%6v error: code=%s msg=%s", time.Since(start).Round(10*time.Millisecond), ev.Code, ev.Message)
			}
			if ev.Event == EventFinal || ev.Event == EventError {
				// 再收一小会儿看 abort_ack 会不会补上
				select {
				case ev2, ok := <-events:
					if ok {
						t.Logf("%6v 最终 event=%s（说明 abort 被串行化到 run 之后才处理）", time.Since(start).Round(10*time.Millisecond), ev2.Event)
					}
				case <-time.After(1500 * time.Millisecond):
					t.Logf("run 结束后 1.5s 仍无 abort_ack")
				}
				return
			}
		case <-timeout:
			t.Fatalf("8s 超时，事件流无终态")
		}
	}
}
