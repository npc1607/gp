package rpc

import (
	"bufio"
	"encoding/json"
	"testing"
	"time"
)

func TestProbe_Timeline(t *testing.T) {
	client, cleanup := probeServer(t, &slowProvider{chunks: 6, delay: 250 * time.Millisecond})
	defer cleanup()

	start := time.Now()
	enc := json.NewEncoder(client)
	_ = enc.Encode(Request{ID: "1", Op: OpRun, Prompt: "hello"})

	sc := bufio.NewScanner(client)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for sc.Scan() {
			var ev Event
			_ = json.Unmarshal(sc.Bytes(), &ev)
			t.Logf("%6s  event=%-14s tool=%-6s text=%q msg=%q",
				time.Since(start).Round(10*time.Millisecond), ev.Event, ev.Tool, ev.Text, ev.Message)
		}
	}()
	select {
	case <-done:
		t.Logf("流已关闭，总耗时 %v", time.Since(start).Round(10*time.Millisecond))
	case <-time.After(12 * time.Second):
		t.Logf("12s 内未结束（流未关闭），说明 run 卡住了")
	}
}
