package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	`
	_, err := s.db.Exec(schema)
	return err
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
