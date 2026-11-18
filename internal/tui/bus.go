package tui

import (
	"strings"
	"sync"
	"time"
)

// QueuedPrompt 描述输入指令总线中待处理的用户指令
type QueuedPrompt struct {
	ID         uint64
	Text       string
	EnqueuedAt time.Time
}

// InputBus 线程安全的输入指令总线队列。
// 支持在 Agent 运行期间持续接收用户输入，支持待处理指令挤压合并（Squeeze/Coalesce）与未处理指令回退（Rollback）。
type InputBus struct {
	mu      sync.Mutex
	nextID  uint64
	pending []QueuedPrompt
}

// NewInputBus 创建输入指令总线实例
func NewInputBus() *InputBus {
	return &InputBus{}
}

// Enqueue 将一条非空用户指令加入待处理队列，返回入队后的条目与当前队列长度
func (b *InputBus) Enqueue(text string) (QueuedPrompt, int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.nextID++
	item := QueuedPrompt{
		ID:         b.nextID,
		Text:       text,
		EnqueuedAt: time.Now(),
	}
	b.pending = append(b.pending, item)
	return item, len(b.pending)
}

// Dequeue 按 FIFO 顺序取出队首待处理指令
func (b *InputBus) Dequeue() (QueuedPrompt, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.pending) == 0 {
		return QueuedPrompt{}, false
	}
	item := b.pending[0]
	b.pending = b.pending[1:]
	return item, true
}

// DequeueSqueezed 将队列中所有待处理指令挤压合并为一条指令并出队返回。
// 返回合并后的指令、被合并的原始条数以及是否存在有效指令。
func (b *InputBus) DequeueSqueezed() (QueuedPrompt, int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := len(b.pending)
	if n == 0 {
		return QueuedPrompt{}, 0, false
	}
	if n == 1 {
		item := b.pending[0]
		b.pending = nil
		return item, 1, true
	}

	parts := make([]string, 0, n)
	for _, it := range b.pending {
		if trimmed := strings.TrimSpace(it.Text); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	first := b.pending[0]
	b.pending = nil

	return QueuedPrompt{
		ID:         first.ID,
		Text:       strings.Join(parts, "\n\n"),
		EnqueuedAt: first.EnqueuedAt,
	}, n, true
}

// SqueezeInPlace 原地挤压合并队列中所有待处理指令为一条，返回被合并的原始条数
func (b *InputBus) SqueezeInPlace() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := len(b.pending)
	if n <= 1 {
		return n
	}
	parts := make([]string, 0, n)
	for _, it := range b.pending {
		if trimmed := strings.TrimSpace(it.Text); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	first := b.pending[0]
	b.pending = []QueuedPrompt{
		{
			ID:         first.ID,
			Text:       strings.Join(parts, "\n\n"),
			EnqueuedAt: first.EnqueuedAt,
		},
	}
	return n
}

// RollbackLast 回退（弹出）最近一条尚未处理的指令，供用户重新编辑或取消
func (b *InputBus) RollbackLast() (QueuedPrompt, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := len(b.pending)
	if n == 0 {
		return QueuedPrompt{}, false
	}
	last := b.pending[n-1]
	b.pending = b.pending[:n-1]
	return last, true
}

// Clear 清空所有待处理指令并返回被清空的条目
func (b *InputBus) Clear() []QueuedPrompt {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.pending) == 0 {
		return nil
	}
	out := make([]QueuedPrompt, len(b.pending))
	copy(out, b.pending)
	b.pending = nil
	return out
}

// Len 返回当前待处理指令数量
func (b *InputBus) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

// Snapshot 返回当前待处理队列的只读快照
func (b *InputBus) Snapshot() []QueuedPrompt {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.pending) == 0 {
		return nil
	}
	out := make([]QueuedPrompt, len(b.pending))
	copy(out, b.pending)
	return out
}
