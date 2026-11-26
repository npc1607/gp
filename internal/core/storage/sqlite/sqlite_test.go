package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gp/internal/core/storage"
)

var ErrIntentional = errors.New("err intentional transaction rollback")

func checkSqliteObjectExists(t *testing.T, db *sql.DB, objType, objName string) {
	t.Helper()
	var count int
	query := "SELECT count(*) FROM sqlite_master WHERE type=? AND name=?;"
	if err := db.QueryRow(query, objType, objName).Scan(&count); err != nil {
		t.Fatalf("checkSqliteObjectExists(type=%q, name=%q): got error %v, want nil", objType, objName, err)
	}
	if got, want := count, 1; got != want {
		t.Fatalf("checkSqliteObjectExists(type=%q, name=%q): got count %d, want count %d", objType, objName, got, want)
	}
}

func queryPragmaString(t *testing.T, db *sql.DB, pragma string) string {
	t.Helper()
	var val string
	if err := db.QueryRow("PRAGMA " + pragma + ";").Scan(&val); err != nil {
		t.Fatalf("queryPragmaString(pragma=%q): got error %v, want nil", pragma, err)
	}
	return val
}

func queryPragmaInt(t *testing.T, db *sql.DB, pragma string) int {
	t.Helper()
	var val int
	if err := db.QueryRow("PRAGMA " + pragma + ";").Scan(&val); err != nil {
		t.Fatalf("queryPragmaInt(pragma=%q): got error %v, want nil", pragma, err)
	}
	return val
}

// TC-STOR-01: SQLite存储初始化与WAL模式校验
func TestSqliteStorage_TC_STOR_01_InitAndWAL(t *testing.T) {
	fn := "sqlite.Open"
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "tc_stor_01.db")

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("%s(path=%q): got error %v, want nil", fn, dbPath, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	// 1. journal_mode == wal
	if got, want := queryPragmaString(t, store.db, "journal_mode"), "wal"; got != want {
		t.Fatalf("%s(path=%q): PRAGMA journal_mode got %q, want %q", fn, dbPath, got, want)
	}

	// 2. busy_timeout == 5000
	if got, want := queryPragmaInt(t, store.db, "busy_timeout"), 5000; got != want {
		t.Fatalf("%s(path=%q): PRAGMA busy_timeout got %d, want %d", fn, dbPath, got, want)
	}

	// 3. tables: entries, documents, task_checkpoints, context_metrics
	requiredTables := []string{"entries", "documents", "task_checkpoints", "context_metrics"}
	for _, table := range requiredTables {
		checkSqliteObjectExists(t, store.db, "table", table)
	}

	// 4. indexes: idx_entries_session, idx_task_checkpoints_status, idx_task_checkpoints_session, idx_context_metrics_session
	requiredIndexes := []string{
		"idx_entries_session",
		"idx_task_checkpoints_status",
		"idx_task_checkpoints_session",
		"idx_context_metrics_session",
	}
	for _, idx := range requiredIndexes {
		checkSqliteObjectExists(t, store.db, "index", idx)
	}
}

// TC-STOR-02: 存储事务原子性与失败回滚
func TestSqliteStorage_TC_STOR_02_AtomicRollback(t *testing.T) {
	fn := "SqliteStorage.Commit"
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "tc_stor_02.db")

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open(path=%q): got error %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	ctx := context.Background()
	sessID := "sess_tc_stor_02"

	// 1. Seed initial baseline data into entries, documents, checkpoints
	err = store.Commit(ctx, func(tx storage.Transaction) error {
		if err := tx.AppendEntry(&storage.Entry{
			ID:        "baseline_entry",
			SessionID: sessID,
			Type:      "message",
			Payload:   []byte(`{"text":"baseline"}`),
			CreatedAt: time.Now(),
		}); err != nil {
			return err
		}
		if err := tx.SetDocument(sessID, "baseline_doc", []byte(`{"version":1}`)); err != nil {
			return err
		}
		return tx.SaveCheckpoint(&storage.TaskCheckpoint{
			TaskID:         "baseline_task",
			SessionID:      sessID,
			TaskType:       "tool",
			Status:         "completed",
			CheckpointData: []byte(`{"turn":0}`),
			UpdatedAt:      time.Now(),
		})
	})
	if err != nil {
		t.Fatalf("%s(seed): got error %v, want nil", fn, err)
	}

	// 2. Perform transaction that modifies/appends all 3 tables and returns ErrIntentional
	err = store.Commit(ctx, func(tx storage.Transaction) error {
		_ = tx.AppendEntry(&storage.Entry{
			ID:        "uncommitted_entry",
			SessionID: sessID,
			Type:      "message",
			Payload:   []byte(`{"text":"should be rolled back"}`),
			CreatedAt: time.Now(),
		})
		_ = tx.SetDocument(sessID, "baseline_doc", []byte(`{"version":2}`))
		_ = tx.SetDocument(sessID, "uncommitted_doc", []byte(`{"version":1}`))
		_ = tx.SaveCheckpoint(&storage.TaskCheckpoint{
			TaskID:         "uncommitted_task",
			SessionID:      sessID,
			TaskType:       "tool",
			Status:         "running",
			CheckpointData: []byte(`{"turn":1}`),
			UpdatedAt:      time.Now(),
		})
		_ = tx.SaveCheckpoint(&storage.TaskCheckpoint{
			TaskID:         "baseline_task",
			SessionID:      sessID,
			TaskType:       "tool",
			Status:         "running",
			CheckpointData: []byte(`{"turn":99}`),
			UpdatedAt:      time.Now(),
		})
		return ErrIntentional
	})

	if !errors.Is(err, ErrIntentional) {
		t.Fatalf("%s(rollback): got error %v, want %v", fn, err, ErrIntentional)
	}

	// 3. Verify entries: only baseline_entry exists
	entries, err := store.GetEntries(sessID)
	if err != nil {
		t.Fatalf("store.GetEntries(sessID=%q): got error %v, want nil", sessID, err)
	}
	if got, want := len(entries), 1; got != want {
		t.Fatalf("store.GetEntries(sessID=%q): got %d entries, want %d", sessID, got, want)
	}
	if got, want := entries[0].ID, "baseline_entry"; got != want {
		t.Fatalf("store.GetEntries(sessID=%q): got entry ID %q, want %q", sessID, got, want)
	}

	// 4. Verify documents: baseline_doc is version 1, uncommitted_doc does not exist
	docPayload, ok, err := store.GetDocument(sessID, "baseline_doc")
	if err != nil || !ok {
		t.Fatalf("store.GetDocument(sessID=%q, key=%q): got ok=%v err=%v, want ok=true err=nil", sessID, "baseline_doc", ok, err)
	}
	if got, want := string(docPayload), `{"version":1}`; got != want {
		t.Fatalf("store.GetDocument(sessID=%q, key=%q): got payload %q, want %q", sessID, "baseline_doc", got, want)
	}
	_, okNew, _ := store.GetDocument(sessID, "uncommitted_doc")
	if okNew {
		t.Fatalf("store.GetDocument(sessID=%q, key=%q): got ok=true, want ok=false", sessID, "uncommitted_doc")
	}

	// 5. Verify checkpoints: baseline_task remains completed, uncommitted_task does not exist
	baseCp, err := store.GetCheckpoint("baseline_task")
	if err != nil || baseCp == nil {
		t.Fatalf("store.GetCheckpoint(taskID=%q): got cp=%v err=%v, want non-nil cp err=nil", "baseline_task", baseCp, err)
	}
	if got, want := baseCp.Status, "completed"; got != want {
		t.Fatalf("store.GetCheckpoint(taskID=%q).Status: got %q, want %q", "baseline_task", got, want)
	}
	newCp, _ := store.GetCheckpoint("uncommitted_task")
	if newCp != nil {
		t.Fatalf("store.GetCheckpoint(taskID=%q): got non-nil cp, want nil", "uncommitted_task")
	}
	unfinished, err := store.ListUnfinishedCheckpoints()
	if err != nil {
		t.Fatalf("store.ListUnfinishedCheckpoints(): got error %v, want nil", err)
	}
	if got, want := len(unfinished), 0; got != want {
		t.Fatalf("store.ListUnfinishedCheckpoints(): got length %d, want %d", got, want)
	}
}

// TC-STOR-03: 存储并发读写数据竞争安全
func TestSqliteStorage_TC_STOR_03_Concurrent(t *testing.T) {
	fn := "SqliteStorage.Concurrent"
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "tc_stor_03.db")

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open(path=%q): got error %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	ctx := context.Background()
	const numGoroutines = 10
	const opsPerGoroutine = 5

	var wg sync.WaitGroup
	errCh := make(chan error, numGoroutines*opsPerGoroutine*2)

	for g := range numGoroutines {
		wg.Add(1)
		gID := g
		go func() {
			defer wg.Done()
			for i := range opsPerGoroutine {
				taskID := fmt.Sprintf("task_g%d_op%d", gID, i)
				sessionID := fmt.Sprintf("sess_g%d", gID)

				// Write operation (Commit)
				err := store.Commit(ctx, func(tx storage.Transaction) error {
					if err := tx.AppendEntry(&storage.Entry{
						ID:        fmt.Sprintf("entry_g%d_op%d", gID, i),
						SessionID: sessionID,
						Type:      "message",
						Payload:   []byte(`{"data":"conc"}`),
						CreatedAt: time.Now(),
					}); err != nil {
						return err
					}
					if err := tx.SetDocument(sessionID, fmt.Sprintf("doc_op%d", i), []byte(`{"n":1}`)); err != nil {
						return err
					}
					return tx.SaveCheckpoint(&storage.TaskCheckpoint{
						TaskID:         taskID,
						SessionID:      sessionID,
						TaskType:       "tool",
						Status:         "running",
						CheckpointData: []byte(`{}`),
						UpdatedAt:      time.Now(),
					})
				})
				if err != nil {
					errCh <- fmt.Errorf("goroutine %d op %d Commit error: %w", gID, i, err)
					return
				}

				// Read operation
				if _, err := store.GetEntries(sessionID); err != nil {
					errCh <- fmt.Errorf("goroutine %d GetEntries error: %w", gID, err)
					return
				}
				if _, _, err := store.GetDocument(sessionID, fmt.Sprintf("doc_op%d", i)); err != nil {
					errCh <- fmt.Errorf("goroutine %d GetDocument error: %w", gID, err)
					return
				}
				if _, err := store.ListUnfinishedCheckpoints(); err != nil {
					errCh <- fmt.Errorf("goroutine %d ListUnfinishedCheckpoints error: %w", gID, err)
					return
				}
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

	// Verify all items are persisted
	unfinished, err := store.ListUnfinishedCheckpoints()
	if err != nil {
		t.Fatalf("%s: store.ListUnfinishedCheckpoints() error %v, want nil", fn, err)
	}
	expectedTotal := numGoroutines * opsPerGoroutine
	if got, want := len(unfinished), expectedTotal; got != want {
		t.Fatalf("%s: got unfinished count %d, want %d", fn, got, want)
	}
}

func TestSqliteStorage_CRUDAndUpsert(t *testing.T) {
	fn := "SqliteStorage.CRUDAndUpsert"
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "crud.db")

	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open(path=%q): got error %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	ctx := context.Background()

	// 1. Append Entry
	err = store.Commit(ctx, func(tx storage.Transaction) error {
		return tx.AppendEntry(&storage.Entry{
			ID:        "e1",
			SessionID: "s1",
			ParentID:  "",
			Type:      "message",
			Payload:   []byte(`{"text":"hello"}`),
			CreatedAt: time.Now(),
		})
	})
	if err != nil {
		t.Fatalf("%s.AppendEntry: got error %v, want nil", fn, err)
	}

	entries, err := store.GetEntries("s1")
	if err != nil || len(entries) != 1 {
		t.Fatalf("%s.GetEntries(s1): got len %d err %v, want len 1 err nil", fn, len(entries), err)
	}
	if got, want := entries[0].ID, "e1"; got != want {
		t.Fatalf("%s.GetEntries(s1): got ID %q, want %q", fn, got, want)
	}

	// 2. Set & Update Document
	err = store.Commit(ctx, func(tx storage.Transaction) error {
		return tx.SetDocument("s1", "settings", []byte(`{"v":1}`))
	})
	if err != nil {
		t.Fatalf("%s.SetDocument: got error %v, want nil", fn, err)
	}
	err = store.Commit(ctx, func(tx storage.Transaction) error {
		return tx.SetDocument("s1", "settings", []byte(`{"v":2}`))
	})
	if err != nil {
		t.Fatalf("%s.UpdateDocument: got error %v, want nil", fn, err)
	}

	payload, ok, err := store.GetDocument("s1", "settings")
	if err != nil || !ok {
		t.Fatalf("%s.GetDocument: got ok=%v err=%v, want ok=true err=nil", fn, ok, err)
	}
	if got, want := string(payload), `{"v":2}`; got != want {
		t.Fatalf("%s.GetDocument: got payload %q, want %q", fn, got, want)
	}

	// 3. Save & Upsert Checkpoint
	err = store.Commit(ctx, func(tx storage.Transaction) error {
		return tx.SaveCheckpoint(&storage.TaskCheckpoint{
			TaskID:         "t1",
			SessionID:      "s1",
			TaskType:       "tool",
			Status:         "running",
			CheckpointData: []byte(`{"turn":1}`),
			UpdatedAt:      time.Now(),
		})
	})
	if err != nil {
		t.Fatalf("%s.SaveCheckpoint: got error %v, want nil", fn, err)
	}

	unfinished, err := store.ListUnfinishedCheckpoints()
	if err != nil || len(unfinished) != 1 {
		t.Fatalf("%s.ListUnfinishedCheckpoints: got len %d err %v, want len 1 err nil", fn, len(unfinished), err)
	}

	// Update checkpoint to completed
	err = store.Commit(ctx, func(tx storage.Transaction) error {
		return tx.SaveCheckpoint(&storage.TaskCheckpoint{
			TaskID:         "t1",
			SessionID:      "s1",
			TaskType:       "tool",
			Status:         "completed",
			CheckpointData: []byte(`{"turn":1}`),
			UpdatedAt:      time.Now(),
		})
	})
	if err != nil {
		t.Fatalf("%s.UpdateCheckpoint: got error %v, want nil", fn, err)
	}

	cp, err := store.GetCheckpoint("t1")
	if err != nil || cp == nil {
		t.Fatalf("%s.GetCheckpoint(t1): got cp=%v err=%v, want non-nil cp err=nil", fn, cp, err)
	}
	if got, want := cp.Status, "completed"; got != want {
		t.Fatalf("%s.GetCheckpoint(t1).Status: got %q, want %q", fn, got, want)
	}

	unfinishedAfter, err := store.ListUnfinishedCheckpoints()
	if err != nil || len(unfinishedAfter) != 0 {
		t.Fatalf("%s.ListUnfinishedCheckpoints(after): got len %d err %v, want len 0 err nil", fn, len(unfinishedAfter), err)
	}
}

func TestSqliteStorage_ContextMetricsCRUDAndTotals(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	for i := 1; i <= 7; i++ {
		m := &storage.ContextMetric{
			SessionID:        "sess_a",
			ChannelID:        "feishu:oc_123",
			TurnIndex:        i,
			StrategyMode:     "e4a_pr1",
			EpochID:          i / 4,
			EpochMaskedCount: i * 2,
			LedgerVersion:    1,
			LedgerActiveCnt:  2,
			LedgerSuperseded: 1,
			LedgerHash:       "deadbeef",
			ContextBuildMs:   1.25 + float64(i)*0.1,
			InputTokens:      int64(1000 + i*100),
			CacheReadTokens:  int64(800 + i*50),
			CacheWriteTokens: 100,
			OutputTokens:     int64(40 + i),
			RecallCalls:      i % 2,
			RestoreCalls:     i % 3,
		}
		if err := store.RecordContextMetric(ctx, m); err != nil {
			t.Fatalf("RecordContextMetric(turn=%d) error = %v", i, err)
		}
		if m.ID == 0 {
			t.Fatalf("expected non-zero inserted ID for turn %d", i)
		}
	}

	// Other session should remain isolated
	if err := store.RecordContextMetric(ctx, &storage.ContextMetric{
		SessionID:    "sess_b",
		TurnIndex:    1,
		StrategyMode: "e4a_pr1",
		RecallCalls:  9,
	}); err != nil {
		t.Fatalf("RecordContextMetric(sess_b) error = %v", err)
	}

	recent, err := store.ListContextMetrics("sess_a", 5)
	if err != nil {
		t.Fatalf("ListContextMetrics error = %v", err)
	}
	if len(recent) != 5 {
		t.Fatalf("expected 5 recent metrics, got %d", len(recent))
	}
	if recent[0].TurnIndex != 7 || recent[4].TurnIndex != 3 {
		t.Fatalf("expected newest-first order [7..3], got first=%d last=%d", recent[0].TurnIndex, recent[4].TurnIndex)
	}
	if recent[0].ChannelID != "feishu:oc_123" || recent[0].LedgerHash != "deadbeef" {
		t.Fatalf("unexpected fields in recent[0]: %+v", recent[0])
	}

	totals, err := store.GetContextMetricTotals("sess_a")
	if err != nil {
		t.Fatalf("GetContextMetricTotals error = %v", err)
	}
	// i=1..7: recall i%2 -> 1,0,1,0,1,0,1 = 4; restore i%3 -> 1,2,0,1,2,0,1 = 7
	if totals.TurnCount != 7 || totals.TotalRecallCalls != 4 || totals.TotalRestoreCalls != 7 {
		t.Fatalf("unexpected totals: %+v, want TurnCount=7 Recall=4 Restore=7", totals)
	}

	if err := store.DeleteSession("sess_a"); err != nil {
		t.Fatalf("DeleteSession error = %v", err)
	}
	afterDel, err := store.ListContextMetrics("sess_a", 5)
	if err != nil {
		t.Fatalf("ListContextMetrics after delete error = %v", err)
	}
	if len(afterDel) != 0 {
		t.Fatalf("expected sess_a metrics deleted, got %d rows", len(afterDel))
	}
	totalsB, err := store.GetContextMetricTotals("sess_b")
	if err != nil || totalsB.TurnCount != 1 || totalsB.TotalRecallCalls != 9 {
		t.Fatalf("expected sess_b untouched, got %+v err=%v", totalsB, err)
	}
}

func TestSqliteStorage_MigratesLegacyContextMetricsSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy_pr1.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	legacyDDL := `
	CREATE TABLE context_metrics (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL,
		channel_id TEXT NOT NULL DEFAULT '',
		turn_index INTEGER NOT NULL,
		strategy_mode TEXT NOT NULL,
		epoch_id INTEGER NOT NULL,
		epoch_masked_count INTEGER NOT NULL,
		ledger_version INTEGER NOT NULL,
		ledger_active_cnt INTEGER NOT NULL,
		ledger_superseded INTEGER NOT NULL,
		ledger_hash TEXT NOT NULL,
		context_build_ms REAL NOT NULL,
		input_tokens INTEGER NOT NULL,
		cache_read_tokens INTEGER NOT NULL,
		cache_write_tokens INTEGER NOT NULL,
		output_tokens INTEGER NOT NULL,
		recall_calls INTEGER NOT NULL,
		restore_calls INTEGER NOT NULL,
		created_at TEXT NOT NULL
	);
	INSERT INTO context_metrics (
		session_id, channel_id, turn_index, strategy_mode, epoch_id, epoch_masked_count,
		ledger_version, ledger_active_cnt, ledger_superseded, ledger_hash,
		context_build_ms, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens,
		recall_calls, restore_calls, created_at
	) VALUES (
		'legacy_sess', 'feishu:oc_old', 1, 'e4a_pr1', 3, 16,
		1, 1, 0, 'abcd1234',
		0.45, 50000, 40000, 0, 120,
		1, 0, '2026-11-26T10:00:00Z'
	);`
	if _, err := rawDB.Exec(legacyDDL); err != nil {
		_ = rawDB.Close()
		t.Fatalf("seed legacy schema error = %v", err)
	}
	_ = rawDB.Close()

	// Re-open with SqliteStorage.Open: must idempotently migrate missing PR #2 columns without data loss.
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open(legacy DB) error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Second Open pass must remain idempotent.
	if err := store.initSchema(); err != nil {
		t.Fatalf("idempotent initSchema retry error = %v", err)
	}

	ctx := context.Background()
	if err := store.RecordContextMetric(ctx, &storage.ContextMetric{
		SessionID:            "legacy_sess",
		ChannelID:            "feishu:oc_old",
		TurnIndex:            2,
		StrategyMode:         "e4a_pr2",
		EpochID:              4,
		EpochMaskedCount:     16,
		LedgerVersion:        1,
		LedgerActiveCnt:      1,
		LedgerSuperseded:     0,
		LedgerHash:           "abcd1234",
		WorkingSetTokens:     920,
		WorkingSetEntries:    4,
		RetrievalBuildMs:     1.85,
		GraphMatchedEntities: 3,
		ContextBuildMs:       2.10,
		InputTokens:          42000,
		CacheReadTokens:      39000,
		OutputTokens:         150,
		RecallCalls:          1,
		RestoreCalls:         0,
		SearchCalls:          2,
	}); err != nil {
		t.Fatalf("RecordContextMetric after migration error = %v", err)
	}

	rows, err := store.ListContextMetrics("legacy_sess", 10)
	if err != nil {
		t.Fatalf("ListContextMetrics error = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows (1 legacy + 1 PR2), got %d", len(rows))
	}
	if rows[0].WorkingSetTokens != 920 || rows[0].WorkingSetEntries != 4 || rows[0].GraphMatchedEntities != 3 || rows[0].SearchCalls != 2 {
		t.Fatalf("unexpected migrated row[0]: %+v", rows[0])
	}
	if rows[1].TurnIndex != 1 || rows[1].SearchCalls != 0 || rows[1].WorkingSetTokens != 0 {
		t.Fatalf("unexpected legacy row[1]: %+v", rows[1])
	}

	totals, err := store.GetContextMetricTotals("legacy_sess")
	if err != nil {
		t.Fatalf("GetContextMetricTotals error = %v", err)
	}
	if totals.TurnCount != 2 || totals.TotalRecallCalls != 2 || totals.TotalSearchCalls != 2 {
		t.Fatalf("unexpected totals: %+v", totals)
	}
}
