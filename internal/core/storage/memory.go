package storage

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

var _ Storage = (*MemoryStorage)(nil)

// MemoryStorage 纯内存存储实现，用于单测与轻量子 Agent
type MemoryStorage struct {
	mu             sync.RWMutex
	entries        map[string][]*Entry             // session_id -> list of entries
	entryByID      map[string]*Entry               // entry_id -> entry
	documents      map[string]map[string]*Document // session_id -> doc_key -> document
	checkpoints    map[string]*TaskCheckpoint      // task_id -> checkpoint
	contextMetrics map[string][]ContextMetric      // session_id -> ordered metrics
	nextMetricID   int64
	closed         bool
}

// NewMemoryStorage 创建新的纯内存存储实例
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		entries:        make(map[string][]*Entry),
		entryByID:      make(map[string]*Entry),
		documents:      make(map[string]map[string]*Document),
		checkpoints:    make(map[string]*TaskCheckpoint),
		contextMetrics: make(map[string][]ContextMetric),
	}
}

func (m *MemoryStorage) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *MemoryStorage) GetEntries(sessionID string) ([]*Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, fmt.Errorf("storage closed")
	}

	src := m.entries[sessionID]
	res := make([]*Entry, len(src))
	for i, e := range src {
		res[i] = cloneEntry(e)
	}
	return res, nil
}

func (m *MemoryStorage) GetDocument(sessionID, docKey string) ([]byte, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, false, fmt.Errorf("storage closed")
	}

	sec, ok := m.documents[sessionID]
	if !ok {
		return nil, false, nil
	}
	doc, ok := sec[docKey]
	if !ok {
		return nil, false, nil
	}
	payloadCopy := make([]byte, len(doc.Payload))
	copy(payloadCopy, doc.Payload)
	return payloadCopy, true, nil
}

func (m *MemoryStorage) GetCheckpoint(taskID string) (*TaskCheckpoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, fmt.Errorf("storage closed")
	}

	cp, ok := m.checkpoints[taskID]
	if !ok {
		return nil, nil
	}
	return cloneCheckpoint(cp), nil
}

func (m *MemoryStorage) ListUnfinishedCheckpoints() ([]*TaskCheckpoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, fmt.Errorf("storage closed")
	}

	var res []*TaskCheckpoint
	for _, cp := range m.checkpoints {
		if cp.Status == "running" {
			res = append(res, cloneCheckpoint(cp))
		}
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].UpdatedAt.Before(res[j].UpdatedAt)
	})
	return res, nil
}

func (m *MemoryStorage) ListCheckpoints(sessionID string) ([]*TaskCheckpoint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, fmt.Errorf("storage closed")
	}

	var res []*TaskCheckpoint
	for _, cp := range m.checkpoints {
		if cp.SessionID == sessionID {
			res = append(res, cloneCheckpoint(cp))
		}
	}
	sort.Slice(res, func(i, j int) bool {
		return res[i].UpdatedAt.Before(res[j].UpdatedAt)
	})
	return res, nil
}

// memoryTx 内存事务暂存区
type memoryTx struct {
	store             *MemoryStorage
	stagedEntries     []*Entry
	stagedDocuments   map[string]map[string]*Document // sessionID -> docKey -> doc
	stagedCheckpoints map[string]*TaskCheckpoint
}

func (m *MemoryStorage) Commit(ctx context.Context, fn func(tx Transaction) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return fmt.Errorf("storage closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tx := &memoryTx{
		store:             m,
		stagedEntries:     make([]*Entry, 0),
		stagedDocuments:   make(map[string]map[string]*Document),
		stagedCheckpoints: make(map[string]*TaskCheckpoint),
	}

	if err := fn(tx); err != nil {
		// Rollback: discard staging buffer
		return err
	}

	// Apply staged entries
	for _, entry := range tx.stagedEntries {
		cloned := cloneEntry(entry)
		m.entries[cloned.SessionID] = append(m.entries[cloned.SessionID], cloned)
		m.entryByID[cloned.ID] = cloned
	}

	// Apply staged documents
	for sID, docs := range tx.stagedDocuments {
		if m.documents[sID] == nil {
			m.documents[sID] = make(map[string]*Document)
		}
		for k, doc := range docs {
			m.documents[sID][k] = cloneDocument(doc)
		}
	}

	// Apply staged checkpoints
	for id, cp := range tx.stagedCheckpoints {
		m.checkpoints[id] = cloneCheckpoint(cp)
	}

	return nil
}

func (tx *memoryTx) AppendEntry(entry *Entry) error {
	if entry == nil || entry.ID == "" {
		return fmt.Errorf("entry ID cannot be empty")
	}
	if entry.SessionID == "" {
		return fmt.Errorf("entry SessionID cannot be empty")
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	tx.stagedEntries = append(tx.stagedEntries, cloneEntry(entry))
	return nil
}

func (tx *memoryTx) SetDocument(sessionID, docKey string, payload []byte) error {
	if sessionID == "" || docKey == "" {
		return fmt.Errorf("sessionID and docKey cannot be empty")
	}
	if tx.stagedDocuments[sessionID] == nil {
		tx.stagedDocuments[sessionID] = make(map[string]*Document)
	}
	payloadCopy := make([]byte, len(payload))
	copy(payloadCopy, payload)

	tx.stagedDocuments[sessionID][docKey] = &Document{
		SessionID: sessionID,
		DocKey:    docKey,
		Payload:   payloadCopy,
		UpdatedAt: time.Now(),
	}
	return nil
}

func (tx *memoryTx) GetDocument(sessionID, docKey string) ([]byte, bool, error) {
	if sec, ok := tx.stagedDocuments[sessionID]; ok {
		if doc, ok := sec[docKey]; ok {
			payloadCopy := make([]byte, len(doc.Payload))
			copy(payloadCopy, doc.Payload)
			return payloadCopy, true, nil
		}
	}
	// Fall back to storage canonical
	sec, ok := tx.store.documents[sessionID]
	if !ok {
		return nil, false, nil
	}
	doc, ok := sec[docKey]
	if !ok {
		return nil, false, nil
	}
	payloadCopy := make([]byte, len(doc.Payload))
	copy(payloadCopy, doc.Payload)
	return payloadCopy, true, nil
}

func (tx *memoryTx) SaveCheckpoint(cp *TaskCheckpoint) error {
	if cp == nil || cp.TaskID == "" {
		return fmt.Errorf("checkpoint TaskID cannot be empty")
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = time.Now()
	}
	tx.stagedCheckpoints[cp.TaskID] = cloneCheckpoint(cp)
	return nil
}

func (tx *memoryTx) GetCheckpoint(taskID string) (*TaskCheckpoint, error) {
	if cp, ok := tx.stagedCheckpoints[taskID]; ok {
		return cloneCheckpoint(cp), nil
	}
	cp, ok := tx.store.checkpoints[taskID]
	if !ok {
		return nil, nil
	}
	return cloneCheckpoint(cp), nil
}

func cloneEntry(e *Entry) *Entry {
	if e == nil {
		return nil
	}
	payloadCopy := bytes.Clone(e.Payload)
	return &Entry{
		ID:        e.ID,
		SessionID: e.SessionID,
		ParentID:  e.ParentID,
		Type:      e.Type,
		Payload:   payloadCopy,
		CreatedAt: e.CreatedAt,
	}
}

func cloneDocument(d *Document) *Document {
	if d == nil {
		return nil
	}
	payloadCopy := bytes.Clone(d.Payload)
	return &Document{
		SessionID: d.SessionID,
		DocKey:    d.DocKey,
		Payload:   payloadCopy,
		UpdatedAt: d.UpdatedAt,
	}
}

func cloneCheckpoint(cp *TaskCheckpoint) *TaskCheckpoint {
	if cp == nil {
		return nil
	}
	dataCopy := bytes.Clone([]byte(cp.CheckpointData))
	return &TaskCheckpoint{
		TaskID:         cp.TaskID,
		SessionID:      cp.SessionID,
		TaskType:       cp.TaskType,
		Status:         cp.Status,
		CheckpointData: dataCopy,
		UpdatedAt:      cp.UpdatedAt,
	}
}

func (m *MemoryStorage) ListSessions(limit int) ([]SessionSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, fmt.Errorf("storage closed")
	}

	var out []SessionSummary
	for sid, list := range m.entries {
		if len(list) == 0 {
			continue
		}
		out = append(out, SessionSummary{
			SessionID:    sid,
			MessageCount: len(list),
			UpdatedAt:    list[len(list)-1].CreatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MemoryStorage) DeleteSession(sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("storage closed")
	}
	for _, e := range m.entries[sessionID] {
		delete(m.entryByID, e.ID)
	}
	delete(m.entries, sessionID)
	delete(m.documents, sessionID)
	delete(m.contextMetrics, sessionID)
	for tid, cp := range m.checkpoints {
		if cp.SessionID == sessionID {
			delete(m.checkpoints, tid)
		}
	}
	return nil
}

// RecordContextMetric 追加一条上下文可观测指标记录
func (m *MemoryStorage) RecordContextMetric(ctx context.Context, metric *ContextMetric) error {
	if metric == nil || metric.SessionID == "" {
		return fmt.Errorf("context metric SessionID cannot be empty")
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return fmt.Errorf("storage closed")
	}
	m.nextMetricID++
	cp := *metric
	cp.ID = m.nextMetricID
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	m.contextMetrics[cp.SessionID] = append(m.contextMetrics[cp.SessionID], cp)
	metric.ID = cp.ID
	metric.CreatedAt = cp.CreatedAt
	return nil
}

// ListContextMetrics 按最近插入倒序返回指定会话的上下文可观测指标
func (m *MemoryStorage) ListContextMetrics(sessionID string, limit int) ([]ContextMetric, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, fmt.Errorf("storage closed")
	}
	list := m.contextMetrics[sessionID]
	if len(list) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > len(list) {
		limit = len(list)
	}
	out := make([]ContextMetric, 0, limit)
	for i := len(list) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, list[i])
	}
	return out, nil
}

// GetContextMetricTotals 汇总指定会话的累计轮次与 recall/restore 调用次数
func (m *MemoryStorage) GetContextMetricTotals(sessionID string) (ContextMetricTotals, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return ContextMetricTotals{}, fmt.Errorf("storage closed")
	}
	list := m.contextMetrics[sessionID]
	totals := ContextMetricTotals{TurnCount: len(list)}
	for _, item := range list {
		totals.TotalRecallCalls += item.RecallCalls
		totals.TotalRestoreCalls += item.RestoreCalls
	}
	return totals, nil
}
