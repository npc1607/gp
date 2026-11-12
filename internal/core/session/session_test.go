package session_test

import (
	"path/filepath"
	"testing"
	"time"

	"gp/internal/ai"
	"gp/internal/core/session"
	"gp/internal/core/storage"
	"gp/internal/core/storage/sqlite"
)

// TC-SESS-01: 会话记录流持久化与重建恢复
func TestSession_TC_SESS_01_PersistenceAndReconstruction(t *testing.T) {
	fn := "session.NewSessionTree"
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "tc_sess_01.db")

	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open(path=%q): got error %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	sessID := "sess_tc_sess_01"
	st, err := session.NewSessionTree(sessID, store)
	if err != nil {
		t.Fatalf("%s(sessionID=%q, store): got error %v, want nil", fn, sessID, err)
	}

	expectedMsgs := []ai.Message{
		{
			Role:      ai.RoleUser,
			Content:   "Message 1: analyze repository structure",
			Timestamp: time.Now(),
		},
		{
			Role:      ai.RoleAssistant,
			Content:   "Message 2: inspecting directory layout",
			Timestamp: time.Now(),
		},
		{
			Role:      ai.RoleUser,
			Content:   "Message 3: please proceed with implementation",
			Timestamp: time.Now(),
		},
	}

	var lastEntryID string
	for i, msg := range expectedMsgs {
		entry := st.AppendMessage(msg)
		if entry == nil || entry.ID == "" {
			t.Fatalf("st.AppendMessage(idx=%d): got nil or empty entry, want valid entry", i)
		}
		lastEntryID = entry.ID
	}

	// 重启恢复会话树
	stReloaded, err := session.NewSessionTree(sessID, store)
	if err != nil {
		t.Fatalf("%s(reload sessionID=%q): got error %v, want nil", fn, sessID, err)
	}

	// 1. 验证叶子节点指向最后一条 Message
	if got, want := stReloaded.ActiveLeafID(), lastEntryID; got != want {
		t.Fatalf("%s: ActiveLeafID got %q, want %q", fn, got, want)
	}

	// 2. 递归追溯消息序列与写入前一致
	reloadedMsgs := stReloaded.GetLinearContext()
	if got, want := len(reloadedMsgs), len(expectedMsgs); got != want {
		t.Fatalf("%s: linear context length got %d, want %d", fn, got, want)
	}

	for i, wantMsg := range expectedMsgs {
		gotMsg := reloadedMsgs[i]
		if gotMsg.Role != wantMsg.Role {
			t.Fatalf("%s: msg[%d].Role got %q, want %q", fn, i, gotMsg.Role, wantMsg.Role)
		}
		if gotMsg.Content != wantMsg.Content {
			t.Fatalf("%s: msg[%d].Content got %q, want %q", fn, i, gotMsg.Content, wantMsg.Content)
		}
	}
}

func TestSession_MemoryStoragePersistence(t *testing.T) {
	fn := "session.NewSessionTree(Memory)"
	store := storage.NewMemoryStorage()
	t.Cleanup(func() {
		_ = store.Close()
	})

	sessID := "sess-mem-1"
	st, err := session.NewSessionTree(sessID, store)
	if err != nil {
		t.Fatalf("%s: init got error %v, want nil", fn, err)
	}

	st.AppendMessage(ai.Message{
		Role:      ai.RoleUser,
		Content:   "Hello, world!",
		Timestamp: time.Now(),
	})
	st.AppendMessage(ai.Message{
		Role:      ai.RoleAssistant,
		Content:   "Hi there!",
		Timestamp: time.Now(),
	})

	// Reload a new session tree from the same store
	stReloaded, err := session.NewSessionTree(sessID, store)
	if err != nil {
		t.Fatalf("%s: reload got error %v, want nil", fn, err)
	}

	msgs := stReloaded.GetLinearContext()
	if got, want := len(msgs), 2; got != want {
		t.Fatalf("%s: linear context length got %d, want %d", fn, got, want)
	}
	if got, want := msgs[0].Content, "Hello, world!"; got != want {
		t.Fatalf("%s: msgs[0].Content got %q, want %q", fn, got, want)
	}
	if got, want := msgs[1].Content, "Hi there!"; got != want {
		t.Fatalf("%s: msgs[1].Content got %q, want %q", fn, got, want)
	}
}
