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

// SessionSummary 历史会话摘要，用于 /resume 浏览与恢复
type SessionSummary struct {
	SessionID    string    `json:"session_id"`
	MessageCount int       `json:"message_count"`
	FirstPrompt  string    `json:"first_prompt"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// SessionLister 可选历史会话列举接口
type SessionLister interface {
	ListSessions(limit int) ([]SessionSummary, error)
}

// SessionDeleter 可选历史会话删除接口
type SessionDeleter interface {
	DeleteSession(sessionID string) error
}

// ContextMetric 记录单轮模型请求的上下文组装与缓存可观测指标
type ContextMetric struct {
	ID               int64     `json:"id,omitempty"`
	SessionID        string    `json:"session_id"`
	ChannelID        string    `json:"channel_id"`
	TurnIndex        int       `json:"turn_index"`
	StrategyMode     string    `json:"strategy_mode"`
	EpochID          int       `json:"epoch_id"`
	EpochMaskedCount int       `json:"epoch_masked_count"`
	LedgerVersion    int       `json:"ledger_version"`
	LedgerActiveCnt  int       `json:"ledger_active_cnt"`
	LedgerSuperseded int       `json:"ledger_superseded"`
	LedgerHash       string    `json:"ledger_hash"`
	ContextBuildMs   float64   `json:"context_build_ms"`
	InputTokens      int64     `json:"input_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	RecallCalls      int       `json:"recall_calls"`
	RestoreCalls     int       `json:"restore_calls"`
	CreatedAt        time.Time `json:"created_at"`
}

// ContextMetricTotals 汇总指定会话的累计轮次与无损回查工具调用次数
type ContextMetricTotals struct {
	TurnCount         int `json:"turn_count"`
	TotalRecallCalls  int `json:"total_recall_calls"`
	TotalRestoreCalls int `json:"total_restore_calls"`
}

// ContextMetricStore 可选上下文可观测性指标读写接口
type ContextMetricStore interface {
	RecordContextMetric(ctx context.Context, m *ContextMetric) error
	ListContextMetrics(sessionID string, limit int) ([]ContextMetric, error)
	GetContextMetricTotals(sessionID string) (ContextMetricTotals, error)
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
