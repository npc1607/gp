package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestEventBus_PubSubWildcardAndHistory(t *testing.T) {
	bus := NewEventBus(8)

	exactCh, unsubExact := bus.Subscribe("test_failed", 4)
	defer unsubExact()

	prefixCh, unsubPrefix := bus.Subscribe("dag.node.*", 8)
	defer unsubPrefix()

	allCh, unsubAll := bus.Subscribe("*", 8)
	defer unsubAll()

	if got := bus.SubscriberCount("dag.node.started"); got != 2 {
		t.Fatalf("SubscriberCount(dag.node.started) = %d, want 2", got)
	}

	bus.Publish(BusEvent{Topic: "dag.node.started", Source: "scout", Payload: "running"})
	bus.Publish(BusEvent{Topic: "test_failed", Source: "verifier", Payload: "exit 1"})

	select {
	case ev := <-exactCh:
		if ev.Topic != "test_failed" || ev.Source != "verifier" {
			t.Fatalf("unexpected exact event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for exact event")
	}

	select {
	case ev := <-prefixCh:
		if ev.Topic != "dag.node.started" {
			t.Fatalf("unexpected prefix event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for prefix event")
	}

	if got := len(allCh); got != 2 {
		t.Fatalf("wildcard '*' channel len = %d, want 2", got)
	}

	hist := bus.History("dag.*", 0)
	if len(hist) != 1 || hist[0].Topic != "dag.node.started" {
		t.Fatalf("History('dag.*') = %+v, want 1 dag.node.started", hist)
	}
}

func TestDAGScheduler_PublishesToEventBus(t *testing.T) {
	hub := NewCoordHub()
	sched := NewDAGScheduler(NewGovernor(4, 10000), hub.Blackboard)
	sched.SetEventBus(hub.EventBus)

	var mu sync.Mutex
	var topics []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unsub := hub.EventBus.SubscribeFunc(ctx, "dag.node.*", func(ev BusEvent) {
		mu.Lock()
		topics = append(topics, ev.Topic)
		mu.Unlock()
	})
	defer unsub()

	_ = sched.AddNode(&TaskNode{ID: "n1", AgentRole: "scout", TaskPrompt: "scan"})
	sched.SetDefaultRunner(func(ctx context.Context, n *TaskNode, input string) (string, error) {
		return "done", nil
	})
	if err := sched.Run(ctx); err != nil {
		t.Fatalf("DAGScheduler.Run() error = %v", err)
	}

	// Give SubscribeFunc goroutine a moment to drain
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(topics)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(topics) < 2 {
		t.Fatalf("expected >= 2 dag.node.* events, got %v", topics)
	}
}
