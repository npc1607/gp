package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gp/internal/core/storage"
	_ "modernc.org/sqlite"
)

var _ storage.Storage = (*SqliteStorage)(nil)

// SqliteStorage 纯 Go SQLite 存储后端，支持 WAL 模式与原子事务
type SqliteStorage struct {
	db      *sql.DB
	writeMu sync.Mutex
	closed  bool
	closeMu sync.RWMutex
}

// Open 打开或创建 SQLite 数据库，并初始化 schema 与 WAL 模式
func Open(dbPath string) (*SqliteStorage, error) {
	if dbPath != ":memory:" && !isInMemoryURI(dbPath) {
		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create db directory %s: %w", dir, err)
		}
	}

	dsn := dbPath
	if dbPath == ":memory:" {
		dsn = "file::memory:?cache=shared"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite db: %w", err)
	}

	// 单连接或控制池大小，避免并发写冲突
	db.SetMaxOpenConns(1)

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA foreign_keys = ON;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("failed to execute %q: %w", p, err)
		}
	}

	s := &SqliteStorage{db: db}
	if err := s.initSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize schema: %w", err)
	}

	return s, nil
}

func isInMemoryURI(path string) bool {
	return path == "file::memory:?cache=shared" || path == ":memory:"
}

func (s *SqliteStorage) initSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS entries (
		id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		parent_id TEXT NOT NULL DEFAULT '',
		entry_type TEXT NOT NULL,
		payload BLOB NOT NULL,
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_entries_session ON entries(session_id, created_at);

	CREATE TABLE IF NOT EXISTS documents (
		session_id TEXT NOT NULL,
		doc_key TEXT NOT NULL,
		payload BLOB NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (session_id, doc_key)
	);

	CREATE TABLE IF NOT EXISTS task_checkpoints (
		task_id TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		task_type TEXT NOT NULL,
		status TEXT NOT NULL,
		checkpoint_data BLOB NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_task_checkpoints_status ON task_checkpoints(status);
	CREATE INDEX IF NOT EXISTS idx_task_checkpoints_session ON task_checkpoints(session_id);

	CREATE TABLE IF NOT EXISTS context_metrics (
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
		working_set_tokens INTEGER NOT NULL DEFAULT 0,
		working_set_entries INTEGER NOT NULL DEFAULT 0,
		retrieval_build_ms REAL NOT NULL DEFAULT 0,
		graph_matched_entities INTEGER NOT NULL DEFAULT 0,
		context_build_ms REAL NOT NULL,
		input_tokens INTEGER NOT NULL,
		cache_read_tokens INTEGER NOT NULL,
		cache_write_tokens INTEGER NOT NULL,
		output_tokens INTEGER NOT NULL,
		recall_calls INTEGER NOT NULL,
		restore_calls INTEGER NOT NULL,
		search_calls INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_context_metrics_session ON context_metrics(session_id, id);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	return s.ensureContextMetricsColumns()
}

func (s *SqliteStorage) ensureContextMetricsColumns() error {
	rows, err := s.db.Query(`PRAGMA table_info(context_metrics)`)
	if err != nil {
		return fmt.Errorf("inspect context_metrics columns: %w", err)
	}
	existing := make(map[string]struct{})
	for rows.Next() {
		var (
			cid      int
			name     string
			ctype    string
			notnull  int
			dflt     sql.NullString
			pk       int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan context_metrics column info: %w", err)
		}
		existing[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()

	migrations := []struct {
		name string
		def  string
	}{
		{name: "channel_id", def: "TEXT NOT NULL DEFAULT ''"},
		{name: "working_set_tokens", def: "INTEGER NOT NULL DEFAULT 0"},
		{name: "working_set_entries", def: "INTEGER NOT NULL DEFAULT 0"},
		{name: "retrieval_build_ms", def: "REAL NOT NULL DEFAULT 0"},
		{name: "graph_matched_entities", def: "INTEGER NOT NULL DEFAULT 0"},
		{name: "search_calls", def: "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, col := range migrations {
		if _, ok := existing[col.name]; ok {
			continue
		}
		if _, err := s.db.Exec(fmt.Sprintf(`ALTER TABLE context_metrics ADD COLUMN %s %s`, col.name, col.def)); err != nil {
			return fmt.Errorf("migrate context_metrics add column %s: %w", col.name, err)
		}
	}
	return nil
}

func (s *SqliteStorage) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

func (s *SqliteStorage) isClosed() bool {
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	return s.closed
}

func (s *SqliteStorage) Commit(ctx context.Context, fn func(tx storage.Transaction) error) error {
	if s.isClosed() {
		return fmt.Errorf("storage closed")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx failed: %w", err)
	}

	var committed bool
	defer func() {
		if !committed {
			_ = sqlTx.Rollback()
		}
	}()

	tx := &sqliteTx{tx: sqlTx}
	if err := fn(tx); err != nil {
		return err
	}

	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("commit tx failed: %w", err)
	}
	committed = true
	return nil
}

func (s *SqliteStorage) GetEntries(sessionID string) ([]*storage.Entry, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("storage closed")
	}

	query := `SELECT id, session_id, parent_id, entry_type, payload, created_at FROM entries WHERE session_id = ? ORDER BY rowid ASC`
	rows, err := s.db.Query(query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query entries failed: %w", err)
	}
	defer rows.Close()

	var entries []*storage.Entry
	for rows.Next() {
		var (
			e         storage.Entry
			createdAt string
		)
		if err := rows.Scan(&e.ID, &e.SessionID, &e.ParentID, &e.Type, &e.Payload, &createdAt); err != nil {
			return nil, fmt.Errorf("scan entry failed: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			e.CreatedAt = t
		}
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}

func (s *SqliteStorage) GetDocument(sessionID, docKey string) ([]byte, bool, error) {
	if s.isClosed() {
		return nil, false, fmt.Errorf("storage closed")
	}

	query := `SELECT payload FROM documents WHERE session_id = ? AND doc_key = ?`
	row := s.db.QueryRow(query, sessionID, docKey)

	var payload []byte
	err := row.Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("query document failed: %w", err)
	}
	return payload, true, nil
}

func (s *SqliteStorage) GetCheckpoint(taskID string) (*storage.TaskCheckpoint, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("storage closed")
	}

	query := `SELECT task_id, session_id, task_type, status, checkpoint_data, updated_at FROM task_checkpoints WHERE task_id = ?`
	row := s.db.QueryRow(query, taskID)

	var (
		cp        storage.TaskCheckpoint
		updatedAt string
	)
	err := row.Scan(&cp.TaskID, &cp.SessionID, &cp.TaskType, &cp.Status, &cp.CheckpointData, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query checkpoint failed: %w", err)
	}
	if t, err := time.Parse(time.RFC3339Nano, updatedAt); err == nil {
		cp.UpdatedAt = t
	}
	return &cp, nil
}

func (s *SqliteStorage) ListUnfinishedCheckpoints() ([]*storage.TaskCheckpoint, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("storage closed")
	}

	query := `SELECT task_id, session_id, task_type, status, checkpoint_data, updated_at FROM task_checkpoints WHERE status = 'running' ORDER BY updated_at ASC`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query unfinished checkpoints failed: %w", err)
	}
	defer rows.Close()

	var res []*storage.TaskCheckpoint
	for rows.Next() {
		var (
			cp        storage.TaskCheckpoint
			updatedAt string
		)
		if err := rows.Scan(&cp.TaskID, &cp.SessionID, &cp.TaskType, &cp.Status, &cp.CheckpointData, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan checkpoint failed: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, updatedAt); err == nil {
			cp.UpdatedAt = t
		}
		res = append(res, &cp)
	}
	return res, rows.Err()
}

func (s *SqliteStorage) ListCheckpoints(sessionID string) ([]*storage.TaskCheckpoint, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("storage closed")
	}

	query := `SELECT task_id, session_id, task_type, status, checkpoint_data, updated_at FROM task_checkpoints WHERE session_id = ? ORDER BY updated_at ASC`
	rows, err := s.db.Query(query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query checkpoints failed: %w", err)
	}
	defer rows.Close()

	var res []*storage.TaskCheckpoint
	for rows.Next() {
		var (
			cp        storage.TaskCheckpoint
			updatedAt string
		)
		if err := rows.Scan(&cp.TaskID, &cp.SessionID, &cp.TaskType, &cp.Status, &cp.CheckpointData, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan checkpoint failed: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, updatedAt); err == nil {
			cp.UpdatedAt = t
		}
		res = append(res, &cp)
	}
	return res, rows.Err()
}

// ListSessions 按最近更新时间倒序列出历史会话摘要
func (s *SqliteStorage) ListSessions(limit int) ([]storage.SessionSummary, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("storage closed")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT session_id, COUNT(*), MAX(created_at) FROM entries GROUP BY session_id ORDER BY MAX(created_at) DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list sessions failed: %w", err)
	}
	defer rows.Close()

	var out []storage.SessionSummary
	for rows.Next() {
		var (
			sum     storage.SessionSummary
			maxTime string
		)
		if err := rows.Scan(&sum.SessionID, &sum.MessageCount, &maxTime); err != nil {
			return nil, fmt.Errorf("scan session summary failed: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, maxTime); err == nil {
			sum.UpdatedAt = t
		}
		out = append(out, sum)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		eRows, err := s.db.Query(
			`SELECT payload FROM entries WHERE session_id = ? AND entry_type = 'message' ORDER BY created_at ASC LIMIT 10`,
			out[i].SessionID,
		)
		if err != nil {
			continue
		}
		for eRows.Next() {
			var payload []byte
			if eRows.Scan(&payload) != nil {
				continue
			}
			var se struct {
				Message *struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(payload, &se) == nil && se.Message != nil && se.Message.Role == "user" && strings.TrimSpace(se.Message.Content) != "" {
				out[i].FirstPrompt = strings.Join(strings.Fields(se.Message.Content), " ")
				break
			}
		}
		_ = eRows.Close()
	}
	return out, nil
}

// sqliteTx SQLite 事务封装
type sqliteTx struct {
	tx *sql.Tx
}

func (tx *sqliteTx) AppendEntry(entry *storage.Entry) error {
	if entry == nil || entry.ID == "" {
		return fmt.Errorf("entry ID cannot be empty")
	}
	if entry.SessionID == "" {
		return fmt.Errorf("entry SessionID cannot be empty")
	}
	createdAt := entry.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	query := `INSERT INTO entries (id, session_id, parent_id, entry_type, payload, created_at) VALUES (?, ?, ?, ?, ?, ?)`
	_, err := tx.tx.Exec(query, entry.ID, entry.SessionID, entry.ParentID, entry.Type, entry.Payload, createdAt.Format(time.RFC3339Nano))
	return err
}

func (tx *sqliteTx) SetDocument(sessionID, docKey string, payload []byte) error {
	if sessionID == "" || docKey == "" {
		return fmt.Errorf("sessionID and docKey cannot be empty")
	}
	now := time.Now().Format(time.RFC3339Nano)
	query := `
	INSERT INTO documents (session_id, doc_key, payload, updated_at)
	VALUES (?, ?, ?, ?)
	ON CONFLICT(session_id, doc_key) DO UPDATE SET
		payload = excluded.payload,
		updated_at = excluded.updated_at
	`
	_, err := tx.tx.Exec(query, sessionID, docKey, payload, now)
	return err
}

func (tx *sqliteTx) GetDocument(sessionID, docKey string) ([]byte, bool, error) {
	query := `SELECT payload FROM documents WHERE session_id = ? AND doc_key = ?`
	row := tx.tx.QueryRow(query, sessionID, docKey)

	var payload []byte
	err := row.Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("tx query document failed: %w", err)
	}
	return payload, true, nil
}

func (tx *sqliteTx) SaveCheckpoint(cp *storage.TaskCheckpoint) error {
	if cp == nil || cp.TaskID == "" {
		return fmt.Errorf("checkpoint TaskID cannot be empty")
	}
	if cp.SessionID == "" {
		return fmt.Errorf("checkpoint SessionID cannot be empty")
	}
	updatedAt := cp.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}

	query := `
	INSERT INTO task_checkpoints (task_id, session_id, task_type, status, checkpoint_data, updated_at)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(task_id) DO UPDATE SET
		session_id = excluded.session_id,
		task_type = excluded.task_type,
		status = excluded.status,
		checkpoint_data = excluded.checkpoint_data,
		updated_at = excluded.updated_at
	`
	raw := cp.CheckpointData
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}

	_, err := tx.tx.Exec(query, cp.TaskID, cp.SessionID, cp.TaskType, cp.Status, []byte(raw), updatedAt.Format(time.RFC3339Nano))
	return err
}

func (tx *sqliteTx) GetCheckpoint(taskID string) (*storage.TaskCheckpoint, error) {
	query := `SELECT task_id, session_id, task_type, status, checkpoint_data, updated_at FROM task_checkpoints WHERE task_id = ?`
	row := tx.tx.QueryRow(query, taskID)

	var (
		cp        storage.TaskCheckpoint
		updatedAt string
	)
	err := row.Scan(&cp.TaskID, &cp.SessionID, &cp.TaskType, &cp.Status, &cp.CheckpointData, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("tx query checkpoint failed: %w", err)
	}
	if t, err := time.Parse(time.RFC3339Nano, updatedAt); err == nil {
		cp.UpdatedAt = t
	}
	return &cp, nil
}

// DeleteSession 删除指定会话的所有条目、文档与任务检查点
func (s *SqliteStorage) DeleteSession(sessionID string) error {
	if s.isClosed() {
		return fmt.Errorf("storage closed")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin delete session: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, q := range []string{
		`DELETE FROM entries WHERE session_id = ?`,
		`DELETE FROM documents WHERE session_id = ?`,
		`DELETE FROM task_checkpoints WHERE session_id = ?`,
		`DELETE FROM context_metrics WHERE session_id = ?`,
	} {
		if _, err := tx.Exec(q, sessionID); err != nil {
			return fmt.Errorf("delete session %q: %w", sessionID, err)
		}
	}
	return tx.Commit()
}

// RecordContextMetric 写入单轮上下文组装与缓存指标至 SQLite context_metrics 表
func (s *SqliteStorage) RecordContextMetric(ctx context.Context, m *storage.ContextMetric) error {
	if s.isClosed() {
		return fmt.Errorf("storage closed")
	}
	if m == nil || m.SessionID == "" {
		return fmt.Errorf("context metric SessionID cannot be empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	createdAt := m.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO context_metrics (
			session_id, channel_id, turn_index, strategy_mode, epoch_id, epoch_masked_count,
			ledger_version, ledger_active_cnt, ledger_superseded, ledger_hash,
			working_set_tokens, working_set_entries, retrieval_build_ms, graph_matched_entities,
			context_build_ms, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens,
			recall_calls, restore_calls, search_calls, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.SessionID, m.ChannelID, m.TurnIndex, m.StrategyMode, m.EpochID, m.EpochMaskedCount,
		m.LedgerVersion, m.LedgerActiveCnt, m.LedgerSuperseded, m.LedgerHash,
		m.WorkingSetTokens, m.WorkingSetEntries, m.RetrievalBuildMs, m.GraphMatchedEntities,
		m.ContextBuildMs, m.InputTokens, m.CacheReadTokens, m.CacheWriteTokens, m.OutputTokens,
		m.RecallCalls, m.RestoreCalls, m.SearchCalls, createdAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return fmt.Errorf("insert context_metric failed: %w", err)
	}
	if id, idErr := res.LastInsertId(); idErr == nil {
		m.ID = id
	}
	m.CreatedAt = createdAt
	return nil
}

// ListContextMetrics 按最近插入倒序查询指定会话的上下文指标（limit <= 0 时默认返回最近 50 条）
func (s *SqliteStorage) ListContextMetrics(sessionID string, limit int) ([]storage.ContextMetric, error) {
	if s.isClosed() {
		return nil, fmt.Errorf("storage closed")
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`
		SELECT id, session_id, channel_id, turn_index, strategy_mode, epoch_id, epoch_masked_count,
			ledger_version, ledger_active_cnt, ledger_superseded, ledger_hash,
			working_set_tokens, working_set_entries, retrieval_build_ms, graph_matched_entities,
			context_build_ms, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens,
			recall_calls, restore_calls, search_calls, created_at
		FROM context_metrics
		WHERE session_id = ?
		ORDER BY id DESC
		LIMIT ?`,
		sessionID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query context_metrics failed: %w", err)
	}
	defer rows.Close()

	var out []storage.ContextMetric
	for rows.Next() {
		var (
			item      storage.ContextMetric
			createdAt string
		)
		if err := rows.Scan(
			&item.ID, &item.SessionID, &item.ChannelID, &item.TurnIndex, &item.StrategyMode,
			&item.EpochID, &item.EpochMaskedCount, &item.LedgerVersion, &item.LedgerActiveCnt,
			&item.LedgerSuperseded, &item.LedgerHash, &item.WorkingSetTokens, &item.WorkingSetEntries,
			&item.RetrievalBuildMs, &item.GraphMatchedEntities, &item.ContextBuildMs, &item.InputTokens,
			&item.CacheReadTokens, &item.CacheWriteTokens, &item.OutputTokens,
			&item.RecallCalls, &item.RestoreCalls, &item.SearchCalls, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan context_metric failed: %w", err)
		}
		if t, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
			item.CreatedAt = t
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// GetContextMetricTotals 汇总指定会话的累计轮次与 recall_entries / restore_observation / search_context 调用次数
func (s *SqliteStorage) GetContextMetricTotals(sessionID string) (storage.ContextMetricTotals, error) {
	if s.isClosed() {
		return storage.ContextMetricTotals{}, fmt.Errorf("storage closed")
	}
	var totals storage.ContextMetricTotals
	err := s.db.QueryRow(`
		SELECT COUNT(*), COALESCE(SUM(recall_calls), 0), COALESCE(SUM(restore_calls), 0), COALESCE(SUM(search_calls), 0)
		FROM context_metrics
		WHERE session_id = ?`,
		sessionID,
	).Scan(&totals.TurnCount, &totals.TotalRecallCalls, &totals.TotalRestoreCalls, &totals.TotalSearchCalls)
	if err != nil {
		return storage.ContextMetricTotals{}, fmt.Errorf("query context_metrics totals failed: %w", err)
	}
	return totals, nil
}
