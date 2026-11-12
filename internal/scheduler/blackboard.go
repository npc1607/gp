package scheduler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Blackboard 线程安全的多 Agent 共享黑板 (事实共享空间)
type Blackboard struct {
	mu          sync.RWMutex
	data        map[string]any
	subscribers map[string][]chan any // key -> watcher channel list; "*" for wildcard
}

// NewBlackboard 创建一个新的线程安全黑板实例
func NewBlackboard() *Blackboard {
	return &Blackboard{
		data:        make(map[string]any),
		subscribers: make(map[string][]chan any),
	}
}

// Set 写入键值数据并触发对该键及通配符的订阅通知
func (b *Blackboard) Set(key string, val any) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.data[key] = val

	// 通知指定 key 的订阅者 (非阻塞投递，避免慢消费者阻塞写锁)
	for _, ch := range b.subscribers[key] {
		select {
		case ch <- val:
		default:
		}
	}

	// 如果非通配符 key，同时通知通配符 "*" 订阅者
	if key != "*" {
		for _, ch := range b.subscribers["*"] {
			select {
			case ch <- val:
			default:
			}
		}
	}
}

// Get 读取指定键的数据
func (b *Blackboard) Get(key string) (any, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	v, ok := b.data[key]
	return v, ok
}

// Has 判断键是否存在
func (b *Blackboard) Has(key string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.data[key]
	return ok
}

// Delete 删除指定键的数据
func (b *Blackboard) Delete(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.data, key)
}

// Keys 返回所有已存储的键名列表 (已排序)
func (b *Blackboard) Keys() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	keys := make([]string, 0, len(b.data))
	for k := range b.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Snapshot 导出当前黑板数据的独立浅拷贝快照
func (b *Blackboard) Snapshot() map[string]any {
	b.mu.RLock()
	defer b.mu.RUnlock()
	snap := make(map[string]any, len(b.data))
	for k, v := range b.data {
		snap[k] = v
	}
	return snap
}

// Watch 订阅指定键（或 "*" 通配符）的变更通知。
// 当 ctx 被取消时，自动清理订阅者并关闭通道。
func (b *Blackboard) Watch(ctx context.Context, key string) <-chan any {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan any, 16)
	b.subscribers[key] = append(b.subscribers[key], ch)

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		defer b.mu.Unlock()

		subs := b.subscribers[key]
		for i, sub := range subs {
			if sub == ch {
				b.subscribers[key] = append(subs[:i], subs[i+1:]...)
				close(ch)
				break
			}
		}
		if len(b.subscribers[key]) == 0 {
			delete(b.subscribers, key)
		}
	}()

	return ch
}

// BuildInputForTask 从黑板提取依赖项并组装成提示词上下文
func (b *Blackboard) BuildInputForTask(dependsOn []string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var sb strings.Builder
	for _, dep := range dependsOn {
		if val, ok := b.data[dep]; ok {
			sb.WriteString(fmt.Sprintf("\n--- 来自依赖节点 [%s] 的事实数据 ---\n%v\n", dep, val))
		}
	}
	return sb.String()
}
