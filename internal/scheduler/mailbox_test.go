package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestMailboxRouter_ActorChannelSendAndPost(t *testing.T) {
	mr := NewMailboxRouter()
	inbox, unreg := mr.RegisterMailbox("Scout", 4)
	defer unreg()

	if !mr.HasPeer("scout") {
		t.Fatalf("expected HasPeer('scout') = true")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Goroutine actor processing mailbox messages
	var postReceived sync.WaitGroup
	postReceived.Add(1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-inbox:
				if msg.Payload == "ping" {
					msg.Reply("pong from "+msg.To, nil)
				} else {
					postReceived.Done()
				}
			}
		}
	}()

	// Synchronous A2A request-reply
	reply, err := mr.Send(ctx, "implementer", "scout", "ping")
	if err != nil {
		t.Fatalf("Send() error = %v, want nil", err)
	}
	if reply != "pong from scout" {
		t.Fatalf("Send() reply = %q, want 'pong from scout'", reply)
	}

	// Async fire-and-forget Post
	if err := mr.Post(ctx, "implementer", "scout", "async-note"); err != nil {
		t.Fatalf("Post() error = %v, want nil", err)
	}
	done := make(chan struct{})
	go func() {
		postReceived.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for async Post message")
	}
}

func TestMailboxRouter_ErrorsAndTimeout(t *testing.T) {
	mr := NewMailboxRouter()
	_, err := mr.Send(context.Background(), "a", "missing", "hi")
	if !errors.Is(err, ErrPeerNotFound) {
		t.Fatalf("Send to missing peer err = %v, want ErrPeerNotFound", err)
	}

	// Unbuffered/slow actor times out on ctx
	_, unreg := mr.RegisterMailbox("slow", 1)
	defer unreg()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = mr.Send(ctx, "a", "slow", "hello")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
}
