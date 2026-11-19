package session

import (
	"context"
	"encoding/json"
	"fmt"
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
				st.entries[se.ID] = &se
				lastID = se.ID
			} else {
				// 兜底反序列化
				se = SessionEntry{
					ID:        e.ID,
					ParentID:  e.ParentID,
					Timestamp: e.CreatedAt,
					Type:      EntryType(e.Type),
				}
				st.entries[se.ID] = &se
				lastID = se.ID
			}
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

func (s *SessionTree) appendEntryLocked(entry *SessionEntry) *SessionEntry {
	entryID := fmt.Sprintf("entry_%d_%d", time.Now().UnixNano(), entrySeq.Add(1))
	now := time.Now()
	entry.ID = entryID
	entry.ParentID = s.activeLeafID
	entry.Timestamp = now

	s.entries[entryID] = entry
	s.activeLeafID = entryID

	if s.store != nil {
		payload, err := json.Marshal(entry)
		if err == nil {
			_ = s.store.Commit(context.Background(), func(tx storage.Transaction) error {
				return tx.AppendEntry(&storage.Entry{
					ID:        entryID,
					SessionID: s.sessionID,
					ParentID:  entry.ParentID,
					Type:      string(entry.Type),
					Payload:   payload,
					CreatedAt: now,
				})
			})
		}
	}

	return entry
}

// GetLinearContext 根据当前活跃叶子自底向上回溯，遇到最近压缩锚点即截断拼装发给大模型的线性历史
func (s *SessionTree) GetLinearContext() []ai.Message {
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
		return tailAfterCompaction
	}

	var out []ai.Message
	if trimmed := strings.TrimSpace(anchor.Summary); trimmed != "" {
		out = append(out, ai.Message{
			Role:    ai.RoleUser,
			Content: "<compacted_context>\n" + trimmed + "\n</compacted_context>",
		})
	}
	if len(anchor.KeptMessages) > 0 {
		out = append(out, anchor.KeptMessages...)
	}
	out = append(out, tailAfterCompaction...)
	return out
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
