package scheduler

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// BusEvent 描述事件总线上的单条广播事件
type BusEvent struct {
	ID        uint64
	Topic     string
	Source    string
	Payload   any
	Metadata  map[string]any
	Timestamp time.Time
}

type busSubscription struct {
	id      uint64
	pattern string
	ch      chan BusEvent
}

// EventBus 拓扑 C：基于 Go Channel 的多 Agent Pub/Sub 事件广播总线。
// 支持精确主题订阅（如 "test_failed"）、前缀通配符（如 "subagent.*"）与全局通配符（"*"）。
type EventBus struct {
	mu          sync.RWMutex
	seq         atomic.Uint64
	subSeq      atomic.Uint64
	subs        map[uint64]*busSubscription
	history     []BusEvent
	historySize int
}

// NewEventBus 创建事件总线实例（historySize <= 0 时默认保留最近 128 条事件）
func NewEventBus(historySize int) *EventBus {
	if historySize <= 0 {
		historySize = 128
	}
	return &EventBus{
		subs:        make(map[uint64]*busSubscription),
		historySize: historySize,
	}
}

// Subscribe 订阅指定主题或通配符模式，返回只读事件通道与取消订阅函数
func (b *EventBus) Subscribe(pattern string, bufSize int) (<-chan BusEvent, func()) {
	if bufSize <= 0 {
		bufSize = 32
	}
	subID := b.subSeq.Add(1)
	sub := &busSubscription{
		id:      subID,
		pattern: strings.TrimSpace(pattern),
		ch:      make(chan BusEvent, bufSize),
	}

	b.mu.Lock()
	b.subs[subID] = sub
	b.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, subID)
			b.mu.Unlock()
		})
	}
	return sub.ch, unsubscribe
}

// SubscribeFunc 启动后台协程监听匹配事件并在 ctx 取消时自动退出
func (b *EventBus) SubscribeFunc(ctx context.Context, pattern string, fn func(BusEvent)) func() {
	if ctx == nil {
		ctx = context.Background()
	}
	ch, unsub := b.Subscribe(pattern, 32)
	go func() {
		defer unsub()
		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-ch:
				if fn != nil {
					fn(ev)
				}
			}
		}
	}()
	return unsub
}

// Publish 向所有匹配的订阅者广播事件（非阻塞投递，避免慢消费者拖慢总线），返回成功投递的订阅者数
func (b *EventBus) Publish(ev BusEvent) int {
	ev.ID = b.seq.Add(1)
	if ev.Timestamp.IsZero() {
		ev.Timestamp = time.Now()
	}
	if ev.Metadata != nil {
		ev.Metadata = maps.Clone(ev.Metadata)
	}

	b.mu.Lock()
	b.history = append(b.history, ev)
	if len(b.history) > b.historySize {
		b.history = b.history[len(b.history)-b.historySize:]
	}
	targets := make([]chan BusEvent, 0, len(b.subs))
	for _, sub := range b.subs {
		if matchTopicPattern(sub.pattern, ev.Topic) {
			targets = append(targets, sub.ch)
		}
	}
	b.mu.Unlock()

	delivered := 0
	for _, ch := range targets {
		select {
		case ch <- ev:
			delivered++
		default:
		}
	}
	return delivered
}

// History 查询匹配主题模式的近期事件快照（limit <= 0 返回全部匹配历史）
func (b *EventBus) History(pattern string, limit int) []BusEvent {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var matched []BusEvent
	for _, ev := range b.history {
		if pattern == "" || matchTopicPattern(pattern, ev.Topic) {
			matched = append(matched, ev)
		}
	}
	if limit > 0 && len(matched) > limit {
		matched = matched[len(matched)-limit:]
	}
	return slices.Clone(matched)
}

// SubscriberCount 返回匹配指定主题的活跃订阅者数量
func (b *EventBus) SubscriberCount(topic string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	count := 0
	for _, sub := range b.subs {
		if topic == "" || matchTopicPattern(sub.pattern, topic) {
			count++
		}
	}
	return count
}

func matchTopicPattern(pattern, topic string) bool {
	if pattern == "*" || pattern == topic {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(topic, prefix)
	}
	return false
}
