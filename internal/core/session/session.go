package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gp/internal/ai"
	"gp/internal/core/storage"
)

var entrySeq atomic.Uint64

// EntryType 会话记录条目类型
type EntryType string

const (
	EntryMessage    EntryType = "message"
	EntryCompaction EntryType = "compaction"
	EntryCustom     EntryType = "custom"
)

// SessionEntry 会话分支树节点
type SessionEntry struct {
	ID        string      `json:"id"`
	ParentID  string      `json:"parent_id"` // 指向父节点，构建有向无环分支树
	Timestamp time.Time   `json:"timestamp"`
	Type         EntryType    `json:"type"`
	Message      *ai.Message  `json:"message,omitempty"`
	Summary      string       `json:"summary,omitempty"`       // 针对压缩条目
	KeptMessages []ai.Message `json:"kept_messages,omitempty"` // 压缩时保留的尾部近期消息
}

// SessionTree 会话分支树与线性投影管理器 (对齐 Pi 的 SessionManager)
type SessionTree struct {
	mu           sync.RWMutex
	sessionID    string
	entries      map[string]*SessionEntry
	activeLeafID string
	isInMemory   bool
	store        storage.Storage
}

// NewInMemorySession 创建纯内存会话树
func NewInMemorySession(sessionID string) *SessionTree {
	rootID := "entry_root"
	root := &SessionEntry{
		ID:        rootID,
		ParentID:  "",
		Timestamp: time.Now(),
		Type:      EntryCustom,
	}
	st := &SessionTree{
		sessionID:    sessionID,
		entries:      make(map[string]*SessionEntry),
		activeLeafID: rootID,
		isInMemory:   true,
	}
	st.entries[rootID] = root
	return st
}

// NewSessionTree 根据持久化存储恢复或初始化会话树
func NewSessionTree(sessionID string, store storage.Storage) (*SessionTree, error) {
	if store == nil {
		return NewInMemorySession(sessionID), nil
	}

	st := &SessionTree{
		sessionID:  sessionID,
		entries:    make(map[string]*SessionEntry),
		isInMemory: false,
		store:      store,
	}

	storedEntries, err := store.GetEntries(sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to load entries for session %s: %w", sessionID, err)
	}

	if len(storedEntries) > 0 {
		var lastID string
		for _, e := range storedEntries {
			var se SessionEntry
			if err := json.Unmarshal(e.Payload, &se); err == nil {
				if se.ID == "" {
					se.ID = e.ID
				}
				if se.ParentID == "" {
					se.ParentID = e.ParentID
				}
				if se.Timestamp.IsZero() {
					se.Timestamp = e.CreatedAt
				}
			} else {
				// 兜底反序列化
				se = SessionEntry{
					ID:        e.ID,
					ParentID:  e.ParentID,
					Timestamp: e.CreatedAt,
					Type:      EntryType(e.Type),
				}
			}
			// 历史断链自愈：若存储中存在指向缺失父节点的孤立节点，自动桥接到前序有效节点，避免历史被截断。
			if se.ParentID != "" && st.entries[se.ParentID] == nil && lastID != "" {
				slog.Warn("repaired dangling session entry parent link",
					"session_id", sessionID,
					"entry_id", se.ID,
					"missing_parent_id", se.ParentID,
					"bridged_parent_id", lastID,
				)
				se.ParentID = lastID
			}
			st.entries[se.ID] = &se
			lastID = se.ID
		}
		st.activeLeafID = lastID
		return st, nil
	}

	// 尚无记录，初始化根节点并落盘（带 sessionID 后缀避免多会话共用同一 SQLite 时主键冲突）
	rootID := fmt.Sprintf("entry_root_%s_%d", sessionID, entrySeq.Add(1))
	now := time.Now()
	root := &SessionEntry{
		ID:        rootID,
		ParentID:  "",
		Timestamp: now,
		Type:      EntryCustom,
	}
	st.entries[rootID] = root
	st.activeLeafID = rootID

	rootPayload, _ := json.Marshal(root)
	err = store.Commit(context.Background(), func(tx storage.Transaction) error {
		return tx.AppendEntry(&storage.Entry{
			ID:        rootID,
			SessionID: sessionID,
			ParentID:  "",
			Type:      string(EntryCustom),
			Payload:   rootPayload,
			CreatedAt: now,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("failed to persist session root: %w", err)
	}

	return st, nil
}

func (s *SessionTree) SessionID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessionID
}

func (s *SessionTree) ActiveLeafID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeLeafID
}

// AppendMessage 追加新消息并推进活跃指针，若配置了持久化存储则同步写入
func (s *SessionTree) AppendMessage(msg ai.Message) *SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.appendEntryLocked(&SessionEntry{
		Type:    EntryMessage,
		Message: &msg,
	})
}

// AppendCompaction 追加一条上下文压缩锚点（保留早期摘要与近期尾部消息，原始历史仍无损留存）
func (s *SessionTree) AppendCompaction(summary string, kept []ai.Message) *SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.appendEntryLocked(&SessionEntry{
		Type:         EntryCompaction,
		Summary:      summary,
		KeptMessages: slices.Clone(kept),
	})
}

// CompactionCount 返回当前会话中累计发生的上下文压缩次数（包含自动压缩、手动压缩与断点恢复的历史压缩）
func (s *SessionTree) CompactionCount() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	count := 0
	for _, e := range s.entries {
		if e != nil && e.Type == EntryCompaction {
			count++
		}
	}
	return count
}

func sanitizeMessageToolCalls(m *ai.Message) {
	if m == nil || len(m.ToolCalls) == 0 {
		return
	}
	sanitized := slices.Clone(m.ToolCalls)
	changed := false
	for i := range sanitized {
		raw := sanitized[i].Function.Arguments
		if len(raw) == 0 {
			sanitized[i].Function.Arguments = json.RawMessage("{}")
			changed = true
			continue
		}
		if !json.Valid(raw) {
			wrapped, _ := json.Marshal(map[string]string{"_raw": string(raw)})
			sanitized[i].Function.Arguments = wrapped
			changed = true
		}
	}
	if changed {
		m.ToolCalls = sanitized
	}
}

func sanitizeEntryForJSON(entry *SessionEntry) {
	if entry == nil {
		return
	}
	sanitizeMessageToolCalls(entry.Message)
	for i := range entry.KeptMessages {
		sanitizeMessageToolCalls(&entry.KeptMessages[i])
	}
}

func (s *SessionTree) appendEntryLocked(entry *SessionEntry) *SessionEntry {
	entryID := fmt.Sprintf("entry_%d_%d", time.Now().UnixNano(), entrySeq.Add(1))
	now := time.Now()
	entry.ID = entryID
	entry.ParentID = s.activeLeafID
	entry.Timestamp = now
	sanitizeEntryForJSON(entry)

	if s.store != nil {
		payload, err := json.Marshal(entry)
		if err != nil {
			slog.Error("failed to marshal session entry; skipping activeLeaf advance",
				"session_id", s.sessionID,
				"entry_id", entryID,
				"err", err,
			)
			return entry
		}
		var commitErr error
		backoffs := []time.Duration{0, 15 * time.Millisecond, 45 * time.Millisecond}
		for _, wait := range backoffs {
			if wait > 0 {
				time.Sleep(wait)
			}
			commitErr = s.store.Commit(context.Background(), func(tx storage.Transaction) error {
				return tx.AppendEntry(&storage.Entry{
					ID:        entryID,
					SessionID: s.sessionID,
					ParentID:  entry.ParentID,
					Type:      string(entry.Type),
					Payload:   payload,
					CreatedAt: now,
				})
			})
			if commitErr == nil {
				break
			}
		}
		if commitErr != nil {
			slog.Error("failed to persist session entry; keeping previous activeLeaf",
				"session_id", s.sessionID,
				"entry_id", entryID,
				"parent_id", entry.ParentID,
				"err", commitErr,
			)
			return entry
		}
	}

	s.entries[entryID] = entry
	s.activeLeafID = entryID
	return entry
}

// GetLinearContext 根据当前活跃叶子自底向上回溯，遇到最近压缩锚点即截断拼装发给大模型的线性历史
func (s *SessionTree) GetLinearContext() []ai.Message {
	msgs, _ := s.GetLinearContextWithOffset()
	return msgs
}

// GetLinearContextWithOffset 返回线性上下文及最近压缩锚点之前被截断的累计历史消息偏移量，
// 用于兼容现有调用方的消息计数偏移查询。
func (s *SessionTree) GetLinearContextWithOffset() ([]ai.Message, int) {
	msgs, priorMsgOffset, _ := s.getLinearContextWithOffsets()
	return msgs, priorMsgOffset
}

// GetLinearContextWithToolOffset 返回线性上下文及最近压缩锚点之前被截断的累计工具调用轮次偏移量，
// 用于在 Compaction 之后仍保持按逻辑工具轮次单调递增的 Epoch 步长边界。
func (s *SessionTree) GetLinearContextWithToolOffset() ([]ai.Message, int) {
	msgs, _, priorToolTurnOffset := s.getLinearContextWithOffsets()
	return msgs, priorToolTurnOffset
}

// GetLinearContextWithOffsets 同时返回线性上下文、历史被截断消息偏移量与历史被截断工具调用轮次偏移量。
func (s *SessionTree) GetLinearContextWithOffsets() ([]ai.Message, int, int) {
	return s.getLinearContextWithOffsets()
}

func (s *SessionTree) getLinearContextWithOffsets() ([]ai.Message, int, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var tailAfterCompaction []ai.Message
	var anchor *SessionEntry
	currID := s.activeLeafID

	for currID != "" {
		entry, exists := s.entries[currID]
		if !exists {
			break
		}
		if entry.Type == EntryCompaction {
			anchor = entry
			break
		}
		if entry.Type == EntryMessage && entry.Message != nil {
			tailAfterCompaction = append(tailAfterCompaction, *entry.Message)
		}
		currID = entry.ParentID
	}

	// 逆序翻转为按时间顺序
	slices.Reverse(tailAfterCompaction)

	if anchor == nil {
		return tailAfterCompaction, 0, 0
	}

	totalBeforeAnchor := 0
	toolTurnsBeforeAnchor := 0
	walkID := anchor.ParentID
	for walkID != "" {
		entry, exists := s.entries[walkID]
		if !exists {
			break
		}
		if entry.Type == EntryMessage && entry.Message != nil {
			totalBeforeAnchor++
			if entry.Message.Role == ai.RoleAssistant && len(entry.Message.ToolCalls) > 0 {
				toolTurnsBeforeAnchor++
			}
		}
		walkID = entry.ParentID
	}

	var out []ai.Message
	hasSummary := false
	if trimmed := strings.TrimSpace(anchor.Summary); trimmed != "" {
		hasSummary = true
		out = append(out, ai.Message{
			Role:    ai.RoleUser,
			Content: "<compacted_context>\n" + trimmed + "\n</compacted_context>",
		})
	}
	keptToolTurns := 0
	if len(anchor.KeptMessages) > 0 {
		out = append(out, anchor.KeptMessages...)
		for _, m := range anchor.KeptMessages {
			if m.Role == ai.RoleAssistant && len(m.ToolCalls) > 0 {
				keptToolTurns++
			}
		}
	}
	out = append(out, tailAfterCompaction...)

	priorMsgOffset := totalBeforeAnchor - len(anchor.KeptMessages)
	if hasSummary {
		priorMsgOffset--
	}
	if priorMsgOffset < 0 {
		priorMsgOffset = 0
	}

	priorToolTurnOffset := toolTurnsBeforeAnchor - keptToolTurns
	if priorToolTurnOffset < 0 {
		priorToolTurnOffset = 0
	}
	return out, priorMsgOffset, priorToolTurnOffset
}

// LinearEntry 将线性上下文中的消息与其原始 SessionEntry.ID 绑定，支持溯源回查。
type LinearEntry struct {
	EntryID string
	Message ai.Message
}

// GetEntry 按 entry_id 只读检索会话树中的原始节点（包括压缩锚点之前的历史条目）。
func (s *SessionTree) GetEntry(entryID string) (*SessionEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, ok := s.entries[entryID]
	if !ok || e == nil {
		return nil, false
	}
	cp := *e
	if e.Message != nil {
		m := *e.Message
		cp.Message = &m
	}
	if len(e.KeptMessages) > 0 {
		cp.KeptMessages = slices.Clone(e.KeptMessages)
	}
	return &cp, true
}

// GetBranchEntries 按时间顺序返回当前活跃分支从根节点到叶子节点的全部 EntryMessage（无损 L0 仓库，不被压缩锚点截断）。
func (s *SessionTree) GetBranchEntries() []LinearEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []LinearEntry
	currID := s.activeLeafID
	for currID != "" {
		entry, exists := s.entries[currID]
		if !exists {
			break
		}
		if entry.Type == EntryMessage && entry.Message != nil {
			out = append(out, LinearEntry{
				EntryID: entry.ID,
				Message: *entry.Message,
			})
		}
		currID = entry.ParentID
	}
	slices.Reverse(out)
	return out
}

// GetLinearEntries 返回带 EntryID 指针的当前有效线性上下文（与 GetLinearContext 保持相同锚点截断语义）。
func (s *SessionTree) GetLinearEntries() []LinearEntry {
	entries, _, _ := s.GetLinearState()
	return entries
}

// GetLinearState 返回当前线性上下文条目、锚点之前的历史工具调用轮次偏移量，以及最近压缩锚点 ID（无锚点时为空）。
func (s *SessionTree) GetLinearState() ([]LinearEntry, int, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var tailAfterCompaction []LinearEntry
	var anchor *SessionEntry
	currID := s.activeLeafID

	for currID != "" {
		entry, exists := s.entries[currID]
		if !exists {
			break
		}
		if entry.Type == EntryCompaction {
			anchor = entry
			break
		}
		if entry.Type == EntryMessage && entry.Message != nil {
			tailAfterCompaction = append(tailAfterCompaction, LinearEntry{
				EntryID: entry.ID,
				Message: *entry.Message,
			})
		}
		currID = entry.ParentID
	}

	slices.Reverse(tailAfterCompaction)

	if anchor == nil {
		return tailAfterCompaction, 0, ""
	}

	toolTurnsBeforeAnchor := 0
	walkID := anchor.ParentID
	for walkID != "" {
		entry, exists := s.entries[walkID]
		if !exists {
			break
		}
		if entry.Type == EntryMessage && entry.Message != nil {
			if entry.Message.Role == ai.RoleAssistant && len(entry.Message.ToolCalls) > 0 {
				toolTurnsBeforeAnchor++
			}
		}
		walkID = entry.ParentID
	}

	var out []LinearEntry
	if trimmed := strings.TrimSpace(anchor.Summary); trimmed != "" {
		out = append(out, LinearEntry{
			EntryID: anchor.ID,
			Message: ai.Message{
				Role:    ai.RoleUser,
				Content: "<compacted_context>\n" + trimmed + "\n</compacted_context>",
			},
		})
	}
	keptToolTurns := 0
	for _, m := range anchor.KeptMessages {
		if m.Role == ai.RoleAssistant && len(m.ToolCalls) > 0 {
			keptToolTurns++
		}
		out = append(out, LinearEntry{
			EntryID: anchor.ID,
			Message: m,
		})
	}
	out = append(out, tailAfterCompaction...)

	priorToolTurnOffset := toolTurnsBeforeAnchor - keptToolTurns
	if priorToolTurnOffset < 0 {
		priorToolTurnOffset = 0
	}
	return out, priorToolTurnOffset, anchor.ID
}

// SwitchBranch 切换活跃分支（支持撤销与重试分支）
func (s *SessionTree) SwitchBranch(leafID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[leafID]; !ok {
		return fmt.Errorf("entry not found: %s", leafID)
	}
	s.activeLeafID = leafID
	return nil
}
