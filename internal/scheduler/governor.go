package scheduler

import (
	"context"
	"sync"
	"time"
)

// Governor 全局资源调度器 (控制 Goroutine 并发度、Token 桶速率)
type Governor struct {
	concurrencySem chan struct{} // 基于缓冲 Channel 实现的高性能信号量
	rateLimiter    *TokenBucket  // 全局每分钟 Token 速率控制器
}

func NewGovernor(maxConcurrency int, tokensPerMin int) *Governor {
	return &Governor{
		concurrencySem: make(chan struct{}, maxConcurrency),
		rateLimiter:    NewTokenBucket(tokensPerMin, time.Minute),
	}
}

// ExecuteSubagent 调度执行子 Agent 协程任务
func (g *Governor) ExecuteSubagent(ctx context.Context, fn func(ctx context.Context) error) error {
	// 1. 获取并发槽位 (防止 Goroutine 数量失控)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case g.concurrencySem <- struct{}{}:
		defer func() { <-g.concurrencySem }()
	}

	// 2. 限流排队
	if err := g.rateLimiter.Wait(ctx, 1); err != nil {
		return err
	}

	// 3. 执行任务逻辑
	return fn(ctx)
}

// TokenBucket 纯标准库实现的轻量 Token 桶限流器
type TokenBucket struct {
	mu         sync.Mutex
	capacity   int
	tokens     int
	refillRate time.Duration
	lastRefill time.Time
}

func NewTokenBucket(capacity int, window time.Duration) *TokenBucket {
	rate := window / time.Duration(capacity)
	return &TokenBucket{
		capacity:   capacity,
		tokens:     capacity,
		refillRate: rate,
		lastRefill: time.Now(),
	}
}

func (tb *TokenBucket) Wait(ctx context.Context, tokens int) error {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	// 补充 Token
	now := time.Now()
	elapsed := now.Sub(tb.lastRefill)
	refillTokens := int(elapsed / tb.refillRate)
	if refillTokens > 0 {
		tb.tokens = min(tb.capacity, tb.tokens+refillTokens)
		tb.lastRefill = now
	}

	if tb.tokens >= tokens {
		tb.tokens -= tokens
		return nil
	}

	// 简化退避等待
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(tb.refillRate):
		return nil
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
