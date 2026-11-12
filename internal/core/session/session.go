package session

import (
	"fmt"
	"sync"
	"time"

	"gp/internal/ai"
)

// EntryType 会话记录条目类型
type EntryType string

const (
	EntryMessage    EntryType = "message"
	EntryCompaction EntryType = "compaction"
	EntryCustom     EntryType = "custom"
)

// SessionEntry 会话分支树节点
type SessionEntry struct {
	ID        string          `json:"id"`
	ParentID  string          `json:"parent_id"` // 指向父节点，构建有向无环分支树
	Timestamp time.Time       `json:"timestamp"`
	Type      EntryType       `json:"type"`
	Message   *ai.Message     `json:"message,omitempty"`
	Summary   string          `json:"summary,omitempty"` // 针对压缩条目
}

// SessionTree 会话分支树与线性投影管理器 (对齐 Pi 的 SessionManager)
type SessionTree struct {
	mu           sync.RWMutex
	sessionID    string
	entries      map[string]*SessionEntry
	activeLeafID string
	isInMemory   bool
}

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

// AppendMessage 追加新消息并推进活跃指针
func (s *SessionTree) AppendMessage(msg ai.Message) *SessionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	entryID := fmt.Sprintf("entry_%d", time.Now().UnixNano())
	entry := &SessionEntry{
		ID:        entryID,
		ParentID:  s.activeLeafID,
		Timestamp: time.Now(),
		Type:      EntryMessage,
		Message:   &msg,
	}

	s.entries[entryID] = entry
	s.activeLeafID = entryID
	return entry
}

// GetLinearContext 根据当前活跃叶子自底向上回溯，重构成发给大模型的线性历史
func (s *SessionTree) GetLinearContext() []ai.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var linear []ai.Message
	currID := s.activeLeafID

	for currID != "" {
		entry, exists := s.entries[currID]
		if !exists {
			break
		}
		if entry.Type == EntryMessage && entry.Message != nil {
			linear = append(linear, *entry.Message)
		}
		currID = entry.ParentID
	}

	// 逆序翻转为按时间顺序
	for i, j := 0, len(linear)-1; i < j; i, j = i+1, j-1 {
		linear[i], linear[j] = linear[j], linear[i]
	}
	return linear
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
