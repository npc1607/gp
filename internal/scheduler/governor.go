package scheduler

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// GovernorStats 资源调度器统计指标
type GovernorStats struct {
	ActiveCount     int   `json:"active_count"`
	QueueDepth      int   `json:"queue_depth"`
	TotalDispatched int64 `json:"total_dispatched"`
}

type slidingWindowEntry struct {
	timestamp time.Time
	tokens    int
}

// SlidingWindowLimiter 基于滑动窗口的精细化速率限制器 (避免 429)
type SlidingWindowLimiter struct {
	mu      sync.Mutex
	window  time.Duration
	limit   int
	entries []slidingWindowEntry
}

// NewSlidingWindowLimiter 创建滑动窗口限流器
func NewSlidingWindowLimiter(limit int, window time.Duration) *SlidingWindowLimiter {
	if limit <= 0 {
		limit = 100000
	}
	if window <= 0 {
		window = time.Minute
	}
	return &SlidingWindowLimiter{
		limit:  limit,
		window: window,
	}
}

// Wait 等待并在滑动窗口内获取指定数量的 Token
func (l *SlidingWindowLimiter) Wait(ctx context.Context, tokens int) error {
	if tokens <= 0 {
		return nil
	}
	if tokens > l.limit {
		return fmt.Errorf("requested tokens %d exceeds limit %d", tokens, l.limit)
	}

	for {
		l.mu.Lock()
		now := time.Now()
		cutoff := now.Add(-l.window)

		// 清理过期条目
		validIdx := 0
		for i, e := range l.entries {
			if e.timestamp.After(cutoff) {
				validIdx = i
				break
			}
			if i == len(l.entries)-1 {
				validIdx = len(l.entries)
			}
		}
		if validIdx > 0 {
			l.entries = l.entries[validIdx:]
		}

		currentTokens := 0
		for _, e := range l.entries {
			currentTokens += e.tokens
		}

		if currentTokens+tokens <= l.limit {
			l.entries = append(l.entries, slidingWindowEntry{
				timestamp: now,
				tokens:    tokens,
			})
			l.mu.Unlock()
			return nil
		}

		// 计算需要释放的 token 数以及需等待的最早时间
		needDrop := currentTokens + tokens - l.limit
		dropped := 0
		var wakeTime time.Time
		for _, e := range l.entries {
			dropped += e.tokens
			wakeTime = e.timestamp.Add(l.window)
			if dropped >= needDrop {
				break
			}
		}
		waitDur := wakeTime.Sub(now)
		if waitDur <= 0 {
			waitDur = time.Millisecond
		}
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitDur):
		}
	}
}

// TokenBucket 兼容别名
type TokenBucket = SlidingWindowLimiter

// NewTokenBucket 兼容构造函数
func NewTokenBucket(capacity int, window time.Duration) *SlidingWindowLimiter {
	return NewSlidingWindowLimiter(capacity, window)
}

// Governor 全局资源调度器 (控制 Goroutine 并发度、Token 滑动窗口速率)
type Governor struct {
	concurrencySem  chan struct{}
	limiter         *SlidingWindowLimiter
	activeCount     int64
	queueDepth      int64
	totalDispatched int64
	maxConcurrency  int
	tokensPerMin    int
}

// NewGovernor 创建默认基于 1 分钟窗口的资源调度器
func NewGovernor(maxConcurrency int, tokensPerMin int) *Governor {
	return NewGovernorWithWindow(maxConcurrency, tokensPerMin, time.Minute)
}

// NewGovernorWithWindow 创建支持自定义时间窗口的资源调度器
func NewGovernorWithWindow(maxConcurrency int, limit int, window time.Duration) *Governor {
	if maxConcurrency <= 0 {
		maxConcurrency = 10
	}
	if limit <= 0 {
		limit = 100000
	}
	return &Governor{
		concurrencySem: make(chan struct{}, maxConcurrency),
		limiter:        NewSlidingWindowLimiter(limit, window),
		maxConcurrency: maxConcurrency,
		tokensPerMin:   limit,
	}
}

// ExecuteSubagent 调度执行子 Agent 协程任务 (消耗 1 个 token)
func (g *Governor) ExecuteSubagent(ctx context.Context, fn func(ctx context.Context) error) error {
	return g.ExecuteWithTokens(ctx, 1, fn)
}

// ExecuteWithTokens 调度执行消耗特定 Token 预算的任务
func (g *Governor) ExecuteWithTokens(ctx context.Context, tokens int, fn func(ctx context.Context) error) error {
	// 1. 排队计数增加
	atomic.AddInt64(&g.queueDepth, 1)

	// 2. 获取并发槽位 (防止 Goroutine 数量失控)
	select {
	case <-ctx.Done():
		atomic.AddInt64(&g.queueDepth, -1)
		return ctx.Err()
	case g.concurrencySem <- struct{}{}:
	}

	// 3. 限流排队 (防止 429)
	if err := g.limiter.Wait(ctx, tokens); err != nil {
		<-g.concurrencySem
		atomic.AddInt64(&g.queueDepth, -1)
		return err
	}

	// 4. 从排队转入活跃执行
	atomic.AddInt64(&g.queueDepth, -1)
	atomic.AddInt64(&g.activeCount, 1)
	atomic.AddInt64(&g.totalDispatched, 1)

	defer func() {
		atomic.AddInt64(&g.activeCount, -1)
		<-g.concurrencySem
	}()

	// 5. 执行任务逻辑
	return fn(ctx)
}

// ActiveCount 当前活跃并发执行中的协程数
func (g *Governor) ActiveCount() int {
	return int(atomic.LoadInt64(&g.activeCount))
}

// QueueDepth 当前正在等待信号量或 Token 的排队协程数
func (g *Governor) QueueDepth() int {
	return int(atomic.LoadInt64(&g.queueDepth))
}

// TotalDispatched 历史上成功进入执行的总任务数
func (g *Governor) TotalDispatched() int64 {
	return atomic.LoadInt64(&g.totalDispatched)
}

// Stats 返回当前调度器的统计观测快照
func (g *Governor) Stats() GovernorStats {
	return GovernorStats{
		ActiveCount:     g.ActiveCount(),
		QueueDepth:      g.QueueDepth(),
		TotalDispatched: g.TotalDispatched(),
	}
}
