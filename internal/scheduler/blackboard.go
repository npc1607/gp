package scheduler

import (
	"fmt"
	"strings"
	"sync"
)

// Blackboard 线程安全的多 Agent 共享黑板 (事实共享空间)
type Blackboard struct {
	mu   sync.RWMutex
	data map[string]any
}

func NewBlackboard() *Blackboard {
	return &Blackboard{
		data: make(map[string]any),
	}
}

func (b *Blackboard) Set(key string, val any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data[key] = val
}

func (b *Blackboard) Get(key string) (any, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	v, ok := b.data[key]
	return v, ok
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
