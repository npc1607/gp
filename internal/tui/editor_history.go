package tui

import "strings"

// InputHistory 维护会话内的输入历史记录与草稿状态，支持上下键回溯
type InputHistory struct {
	entries  []string
	cursor   int
	browsing bool
	draft    string
}

// NewInputHistory 创建输入历史管理器
func NewInputHistory() *InputHistory {
	return &InputHistory{}
}

// Push 记录一条新提交的输入（自动忽略空串与连续重复项），并重置游标
func (h *InputHistory) Push(text string) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return
	}
	if len(h.entries) == 0 || h.entries[len(h.entries)-1] != trimmed {
		h.entries = append(h.entries, trimmed)
	}
	h.ResetCursor()
}

// ResetCursor 重置浏览游标回到新输入态
func (h *InputHistory) ResetCursor() {
	h.cursor = len(h.entries)
	h.browsing = false
	h.draft = ""
}

// IsBrowsing 返回当前是否处于历史浏览态
func (h *InputHistory) IsBrowsing() bool {
	return h.browsing
}

// Len 返回历史条目总数
func (h *InputHistory) Len() int {
	return len(h.entries)
}

// Prev 切换到上一条历史输入（首次进入时保留当前草稿）
func (h *InputHistory) Prev(current string) (string, bool) {
	if len(h.entries) == 0 {
		return "", false
	}
	if !h.browsing {
		h.draft = current
		h.browsing = true
		h.cursor = len(h.entries) - 1
		return h.entries[h.cursor], true
	}
	if h.cursor > 0 {
		h.cursor--
	}
	return h.entries[h.cursor], true
}

// Next 切换到下一条历史输入；到达末尾时恢复进入前的草稿
func (h *InputHistory) Next() (string, bool) {
	if !h.browsing {
		return "", false
	}
	if h.cursor < len(h.entries)-1 {
		h.cursor++
		return h.entries[h.cursor], true
	}
	draft := h.draft
	h.ResetCursor()
	return draft, true
}
