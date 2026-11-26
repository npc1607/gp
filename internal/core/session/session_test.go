package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
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

func TestSession_GetEntryAndBranchEntriesAfterCompaction(t *testing.T) {
	st := session.NewInMemorySession("sess-branch-1")
	e1 := st.AppendMessage(ai.NewUserMessage("early constraint"))
	e2 := st.AppendMessage(ai.NewAssistantMessage("noted", ""))
	st.AppendCompaction("summary", []ai.Message{ai.NewUserMessage("kept")})
	e3 := st.AppendMessage(ai.NewUserMessage("latest turn"))

	got1, ok := st.GetEntry(e1.ID)
	if !ok || got1.Message == nil || got1.Message.Content != "early constraint" {
		t.Fatalf("GetEntry(%q) failed: ok=%v got=%+v", e1.ID, ok, got1)
	}

	branch := st.GetBranchEntries()
	if len(branch) != 3 {
		t.Fatalf("expected 3 lossless EntryMessage entries on branch, got %d", len(branch))
	}
	if branch[0].EntryID != e1.ID || branch[1].EntryID != e2.ID || branch[2].EntryID != e3.ID {
		t.Fatalf("unexpected branch entry IDs: %+v", branch)
	}

	linear := st.GetLinearEntries()
	if len(linear) != 3 {
		t.Fatalf("expected 3 linear entries (summary + kept + latest), got %d", len(linear))
	}
	if linear[2].EntryID != e3.ID {
		t.Fatalf("expected linear[2].EntryID=%q, got %q", e3.ID, linear[2].EntryID)
	}
}

func TestSession_AppendMessage_MalformedToolArgumentsDoesNotBreakChain(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "malformed_args.db"))
	if err != nil {
		t.Fatalf("sqlite.Open error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sessID := "sess-malformed-tool"
	st, err := session.NewSessionTree(sessID, store)
	if err != nil {
		t.Fatalf("NewSessionTree error = %v", err)
	}

	st.AppendMessage(ai.NewUserMessage("insert feishu blocks"))
	st.AppendMessage(ai.Message{
		Role: ai.RoleAssistant,
		ToolCalls: []ai.ToolCall{{
			ID:   "call_bad_args",
			Type: "function",
			Function: ai.FunctionCall{
				Name:      "feishu_insert_blocks",
				Arguments: json.RawMessage(`{"block_id": "broken_truncated_json`),
			},
		}},
	})
	st.AppendMessage(ai.Message{
		Role:       ai.RoleTool,
		ToolCallID: "call_bad_args",
		Content:    "MCP execution error: marshal params",
	})
	st.AppendMessage(ai.NewUserMessage("continue next step"))

	reloaded, err := session.NewSessionTree(sessID, store)
	if err != nil {
		t.Fatalf("reload NewSessionTree error = %v", err)
	}
	msgs := reloaded.GetLinearContext()
	if len(msgs) != 4 {
		t.Fatalf("expected all 4 messages persisted without chain break, got %d", len(msgs))
	}
	if len(msgs[1].ToolCalls) != 1 || !json.Valid(msgs[1].ToolCalls[0].Function.Arguments) {
		t.Fatalf("expected sanitized valid JSON arguments, got %q", string(msgs[1].ToolCalls[0].Function.Arguments))
	}
	if !strings.Contains(string(msgs[1].ToolCalls[0].Function.Arguments), "broken_truncated_json") {
		t.Fatalf("expected raw truncated text preserved inside _raw wrapper, got %q", string(msgs[1].ToolCalls[0].Function.Arguments))
	}
}

func TestSession_NewSessionTree_RepairsDanglingParentID(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "dangling_parent.db"))
	if err != nil {
		t.Fatalf("sqlite.Open error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	sessID := "sess-dangling"
	baseTime := time.Now().Add(-time.Minute)

	writeRaw := func(id, parentID string, msg ai.Message, offset int) {
		se := session.SessionEntry{
			ID:        id,
			ParentID:  parentID,
			Timestamp: baseTime.Add(time.Duration(offset) * time.Second),
			Type:      session.EntryMessage,
			Message:   &msg,
		}
		payload, _ := json.Marshal(se)
		if err := store.Commit(ctx, func(tx storage.Transaction) error {
			return tx.AppendEntry(&storage.Entry{
				ID:        id,
				SessionID: sessID,
				ParentID:  parentID,
				Type:      string(session.EntryMessage),
				Payload:   payload,
				CreatedAt: se.Timestamp,
			})
		}); err != nil {
			t.Fatalf("Commit(%s) error = %v", id, err)
		}
	}

	writeRaw("e_root", "", ai.NewUserMessage("msg 1"), 1)
	writeRaw("e_2", "e_root", ai.NewAssistantMessage("msg 2", ""), 2)
	// Simulate e_missing failing to persist while e_3 still points parent_id to e_missing
	writeRaw("e_3", "e_missing_220", ai.NewUserMessage("msg 3"), 3)
	writeRaw("e_4", "e_3", ai.NewAssistantMessage("msg 4", ""), 4)

	reloaded, err := session.NewSessionTree(sessID, store)
	if err != nil {
		t.Fatalf("NewSessionTree error = %v", err)
	}
	msgs, off := reloaded.GetLinearContextWithOffset()
	if off != 0 || len(msgs) != 4 {
		t.Fatalf("expected repaired chain of 4 messages (off=0), got len=%d off=%d", len(msgs), off)
	}
	if msgs[0].Content != "msg 1" || msgs[3].Content != "msg 4" {
		t.Fatalf("unexpected repaired message order: %+v", msgs)
	}
}

type failingCommitStorage struct {
	storage.Storage
	failNext int
}

func (f *failingCommitStorage) Commit(ctx context.Context, fn func(tx storage.Transaction) error) error {
	if f.failNext > 0 {
		f.failNext--
		return errors.New("SQLITE_BUSY: database is locked")
	}
	return f.Storage.Commit(ctx, fn)
}

func TestSession_AppendMessage_RetriesTransientCommitAndDoesNotLeaveOrphanOnFailure(t *testing.T) {
	inner := storage.NewMemoryStorage()
	t.Cleanup(func() { _ = inner.Close() })

	// 1. Transient failure (fails 2 times, succeeds on 3rd attempt)
	transientStore := &failingCommitStorage{Storage: inner}
	st, err := session.NewSessionTree("sess-retry", transientStore)
	if err != nil {
		t.Fatalf("NewSessionTree error = %v", err)
	}
	transientStore.failNext = 2
	st.AppendMessage(ai.NewUserMessage("first"))
	st.AppendMessage(ai.NewAssistantMessage("second", ""))
	if got := len(st.GetLinearContext()); got != 2 {
		t.Fatalf("expected 2 entries after transient retries, got %d", got)
	}

	// 2. Permanent failure (failNext = 10 > 3 attempts): activeLeafID must NOT advance to unpersisted entry!
	transientStore.failNext = 10
	prevLeaf := st.ActiveLeafID()
	st.AppendMessage(ai.NewUserMessage("should fail persist"))
	if st.ActiveLeafID() != prevLeaf {
		t.Fatalf("expected ActiveLeafID to remain %q on persistent Commit failure, got %q", prevLeaf, st.ActiveLeafID())
	}
	// Once DB recovers, next entry chains off prevLeaf cleanly without creating a broken link!
	transientStore.failNext = 0
	st.AppendMessage(ai.NewUserMessage("after recovery"))
	reloaded, err := session.NewSessionTree("sess-retry", inner)
	if err != nil {
		t.Fatalf("reload error = %v", err)
	}
	msgs := reloaded.GetLinearContext()
	if len(msgs) != 3 || msgs[2].Content != "after recovery" {
		t.Fatalf("expected intact chain of 3 entries after recovery, got %+v", msgs)
	}
}

func TestSession_GetLinearContextWithToolOffset(t *testing.T) {
	store := storage.NewMemoryStorage()
	t.Cleanup(func() { _ = store.Close() })

	st, err := session.NewSessionTree("sess-tool-off", store)
	if err != nil {
		t.Fatalf("NewSessionTree error = %v", err)
	}

	st.AppendMessage(ai.NewUserMessage("start"))
	for i := 0; i < 4; i++ {
		st.AppendMessage(ai.Message{
			Role: ai.RoleAssistant,
			ToolCalls: []ai.ToolCall{{
				ID:   string(rune('a' + i)),
				Type: "function",
			}},
		})
		st.AppendMessage(ai.Message{
			Role:       ai.RoleTool,
			ToolCallID: string(rune('a' + i)),
			Content:    "ok",
		})
	}
	st.AppendMessage(ai.NewAssistantMessage("summary checkpoint", ""))

	before := st.GetLinearContext()
	// Keep the last 3 messages (1 tool-calling assistant, 1 tool result, 1 final assistant).
	kept := before[len(before)-3:]
	st.AppendCompaction("Compacted initial 3 tool turns", kept)

	// Append 2 more tool-calling turns after compaction.
	for i := 0; i < 2; i++ {
		st.AppendMessage(ai.Message{
			Role: ai.RoleAssistant,
			ToolCalls: []ai.ToolCall{{
				ID:   string(rune('x' + i)),
				Type: "function",
			}},
		})
		st.AppendMessage(ai.Message{
			Role:       ai.RoleTool,
			ToolCallID: string(rune('x' + i)),
			Content:    "ok2",
		})
	}

	msgs, msgOff, toolOff := st.GetLinearContextWithOffsets()
	msgsToolOnly, toolOnly := st.GetLinearContextWithToolOffset()
	if toolOff != 3 || toolOnly != 3 {
		t.Fatalf("expected priorToolTurnOffset=3 (4 pre-compaction minus 1 kept), got %d / %d", toolOff, toolOnly)
	}
	if msgOff <= 0 || len(msgs) != len(msgsToolOnly) {
		t.Fatalf("unexpected offsets: msgOff=%d len=%d/%d", msgOff, len(msgs), len(msgsToolOnly))
	}
}
