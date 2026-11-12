package storage_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"gp/internal/core/storage"
)

func TestMemoryStorage_CRUD(t *testing.T) {
	fn := "MemoryStorage.CRUD"
	store := storage.NewMemoryStorage()
	t.Cleanup(func() {
		_ = store.Close()
	})
	ctx := context.Background()

	// 1. Test Entry Append & Get
	err := store.Commit(ctx, func(tx storage.Transaction) error {
		return tx.AppendEntry(&storage.Entry{
			ID:        "e1",
			SessionID: "s1",
			Type:      "message",
			Payload:   []byte(`{"text":"hello"}`),
			CreatedAt: time.Now(),
		})
	})
	if err != nil {
		t.Fatalf("%s.Commit(AppendEntry): got error %v, want nil", fn, err)
	}

	entries, err := store.GetEntries("s1")
	if err != nil {
		t.Fatalf("%s.GetEntries(s1): got error %v, want nil", fn, err)
	}
	if got, want := len(entries), 1; got != want {
		t.Fatalf("%s.GetEntries(s1): got len %d, want %d", fn, got, want)
	}
	if got, want := entries[0].ID, "e1"; got != want {
		t.Fatalf("%s.GetEntries(s1): got entry ID %q, want %q", fn, got, want)
	}

	// 2. Test Document Set & Get
	err = store.Commit(ctx, func(tx storage.Transaction) error {
		return tx.SetDocument("s1", "settings", []byte(`{"theme":"dark"}`))
	})
	if err != nil {
		t.Fatalf("%s.Commit(SetDocument): got error %v, want nil", fn, err)
	}

	doc, ok, err := store.GetDocument("s1", "settings")
	if err != nil || !ok {
		t.Fatalf("%s.GetDocument(s1, settings): got ok=%v err=%v, want ok=true err=nil", fn, ok, err)
	}
	if got, want := string(doc), `{"theme":"dark"}`; got != want {
		t.Fatalf("%s.GetDocument(s1, settings): got payload %q, want %q", fn, got, want)
	}

	// 3. Test Checkpoint Save & List
	err = store.Commit(ctx, func(tx storage.Transaction) error {
		return tx.SaveCheckpoint(&storage.TaskCheckpoint{
			TaskID:         "t1",
			SessionID:      "s1",
			TaskType:       "tool",
			Status:         "running",
			CheckpointData: []byte(`{"call_id":"c1"}`),
			UpdatedAt:      time.Now(),
		})
	})
	if err != nil {
		t.Fatalf("%s.Commit(SaveCheckpoint): got error %v, want nil", fn, err)
	}

	cp, err := store.GetCheckpoint("t1")
	if err != nil || cp == nil {
		t.Fatalf("%s.GetCheckpoint(t1): got cp=%v err=%v, want non-nil cp err=nil", fn, cp, err)
	}
	if got, want := cp.Status, "running"; got != want {
		t.Fatalf("%s.GetCheckpoint(t1).Status: got %q, want %q", fn, got, want)
	}

	unfinished, err := store.ListUnfinishedCheckpoints()
	if err != nil {
		t.Fatalf("%s.ListUnfinishedCheckpoints(): got error %v, want nil", fn, err)
	}
	if got, want := len(unfinished), 1; got != want {
		t.Fatalf("%s.ListUnfinishedCheckpoints(): got len %d, want %d", fn, got, want)
	}
	if got, want := unfinished[0].TaskID, "t1"; got != want {
		t.Fatalf("%s.ListUnfinishedCheckpoints(): got task ID %q, want %q", fn, got, want)
	}
}

func TestMemoryStorage_Rollback(t *testing.T) {
	fn := "MemoryStorage.Rollback"
	store := storage.NewMemoryStorage()
	t.Cleanup(func() {
		_ = store.Close()
	})
	ctx := context.Background()

	errExpected := errors.New("simulated error")
	err := store.Commit(ctx, func(tx storage.Transaction) error {
		_ = tx.AppendEntry(&storage.Entry{
			ID:        "e_fail",
			SessionID: "s_fail",
			Type:      "message",
			Payload:   []byte("never persist"),
		})
		return errExpected
	})

	if !errors.Is(err, errExpected) {
		t.Fatalf("%s: got error %v, want %v", fn, err, errExpected)
	}

	entries, err := store.GetEntries("s_fail")
	if err != nil {
		t.Fatalf("%s: GetEntries error %v, want nil", fn, err)
	}
	if got, want := len(entries), 0; got != want {
		t.Fatalf("%s: got %d entries after rollback, want %d", fn, got, want)
	}
}

func TestMemoryStorage_Concurrent(t *testing.T) {
	fn := "MemoryStorage.Concurrent"
	store := storage.NewMemoryStorage()
	t.Cleanup(func() {
		_ = store.Close()
	})
	ctx := context.Background()

	var wg sync.WaitGroup
	errCh := make(chan error, 40)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		idx := i
		go func() {
			defer wg.Done()
			err := store.Commit(ctx, func(tx storage.Transaction) error {
				return tx.SaveCheckpoint(&storage.TaskCheckpoint{
					TaskID:    fmt.Sprintf("task_%d", idx),
					SessionID: "s_conc",
					TaskType:  "tool",
					Status:    "running",
					UpdatedAt: time.Now(),
				})
			})
			if err != nil {
				errCh <- fmt.Errorf("Commit error: %w", err)
				return
			}
			if _, err := store.ListUnfinishedCheckpoints(); err != nil {
				errCh <- fmt.Errorf("ListUnfinishedCheckpoints error: %w", err)
				return
			}
		}()
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			t.Fatalf("%s: concurrent operation failed: %v", fn, err)
		}
	}
}
