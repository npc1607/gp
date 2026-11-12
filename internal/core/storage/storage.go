package storage

import (
	"context"
	"encoding/json"
	"time"
)

// Entry 不可变会话记录流条目
type Entry struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	ParentID  string    `json:"parent_id,omitempty"`
	Type      string    `json:"entry_type"`
	Payload   []byte    `json:"payload"`
	CreatedAt time.Time `json:"created_at"`
}

// Document Typed JSON 键值状态
type Document struct {
	SessionID string    `json:"session_id"`
	DocKey    string    `json:"doc_key"`
	Payload   []byte    `json:"payload"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TaskCheckpoint 任务执行检查点
type TaskCheckpoint struct {
	TaskID         string          `json:"task_id"`
	SessionID      string          `json:"session_id"`
	TaskType       string          `json:"task_type"`
	Status         string          `json:"status"` // running, completed, failed, aborted, interrupted
	CheckpointData json.RawMessage `json:"checkpoint_data"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// Storage 存储抽象接口
type Storage interface {
	Commit(ctx context.Context, fn func(tx Transaction) error) error
	Close() error
	GetEntries(sessionID string) ([]*Entry, error)
	GetDocument(sessionID, docKey string) ([]byte, bool, error)
	GetCheckpoint(taskID string) (*TaskCheckpoint, error)
	ListUnfinishedCheckpoints() ([]*TaskCheckpoint, error)
	ListCheckpoints(sessionID string) ([]*TaskCheckpoint, error)
}

// Transaction 事务契约接口
type Transaction interface {
	AppendEntry(entry *Entry) error
	SetDocument(sessionID, docKey string, payload []byte) error
	GetDocument(sessionID, docKey string) ([]byte, bool, error)
	SaveCheckpoint(cp *TaskCheckpoint) error
	GetCheckpoint(taskID string) (*TaskCheckpoint, error)
}
