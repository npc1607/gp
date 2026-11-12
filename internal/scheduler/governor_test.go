package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TC-GOV-01: Governor高精度并发信号量限制
func TestGovernor_TC_GOV_01_ConcurrencySemaphore(t *testing.T) {
	fn := "Governor.ExecuteSubagent"
	maxConcurrency := 3
	totalTasks := 20
	inputs := map[string]int{
		"maxConcurrency": maxConcurrency,
		"totalTasks":     totalTasks,
	}

	gov := NewGovernor(maxConcurrency, 100000)

	var currentActive int64
	var maxObservedActive int64
	var completed int64
	var wg sync.WaitGroup

	for i := 0; i < totalTasks; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := gov.ExecuteSubagent(context.Background(), func(ctx context.Context) error {
				curr := atomic.AddInt64(&currentActive, 1)
				for {
					max := atomic.LoadInt64(&maxObservedActive)
					if curr > max {
						if atomic.CompareAndSwapInt64(&maxObservedActive, max, curr) {
							break
						}
					} else {
						break
					}
				}

				time.Sleep(15 * time.Millisecond)
				atomic.AddInt64(&currentActive, -1)
				atomic.AddInt64(&completed, 1)
				return nil
			})
			if err != nil {
				t.Errorf("%s(inputs=%+v): got unexpected task error %v, want nil", fn, inputs, err)
			}
		}()
	}

	wg.Wait()

	if maxObservedActive > int64(maxConcurrency) {
		t.Fatalf("%s(inputs=%+v): got maxObservedActive=%d, want <= %d", fn, inputs, maxObservedActive, maxConcurrency)
	}
	if got, want := completed, int64(totalTasks); got != want {
		t.Fatalf("%s(inputs=%+v): got completed=%d, want %d", fn, inputs, got, want)
	}
	if got, want := gov.ActiveCount(), 0; got != want {
		t.Fatalf("%s(inputs=%+v): got ActiveCount=%d, want %d", fn, inputs, got, want)
	}
	if got, want := gov.QueueDepth(), 0; got != want {
		t.Fatalf("%s(inputs=%+v): got QueueDepth=%d, want %d", fn, inputs, got, want)
	}
	if got, want := gov.TotalDispatched(), int64(totalTasks); got != want {
		t.Fatalf("%s(inputs=%+v): got TotalDispatched=%d, want %d", fn, inputs, got, want)
	}
}

// TC-GOV-02: Governor单次申请超限额拦截
func TestGovernor_TC_GOV_02_ExceedLimitRejection(t *testing.T) {
	fn := "SlidingWindowLimiter.Wait"
	limit := 1000
	requestedTokens := 1001
	inputs := map[string]int{
		"limit":           limit,
		"requestedTokens": requestedTokens,
	}

	gov := NewGovernor(5, limit)
	ctx := context.Background()

	start := time.Now()
	err := gov.limiter.Wait(ctx, requestedTokens)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("%s(inputs=%+v): got error nil, want error containing 'exceeds limit'", fn, inputs)
	}
	wantSubstr := "exceeds limit"
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("%s(inputs=%+v): got error %q, want containing %q", fn, inputs, err.Error(), wantSubstr)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("%s(inputs=%+v): got elapsed %v, want non-blocking immediate return", fn, inputs, elapsed)
	}
}

// TC-GOV-03: Governor排队上下文取消回收
func TestGovernor_TC_GOV_03_QueueContextCancelRecycle(t *testing.T) {
	fn := "Governor.ExecuteSubagent"
	gov := NewGovernor(1, 1000)

	started := make(chan struct{})
	blockerDone := make(chan struct{})

	go func() {
		_ = gov.ExecuteSubagent(context.Background(), func(ctx context.Context) error {
			close(started)
			<-blockerDone
			return nil
		})
	}()

	<-started

	// 饱和状态下入队任务并触发超时/取消
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	inputs := "context_with_timeout_25ms"
	err := gov.ExecuteSubagent(ctx, func(c context.Context) error {
		return nil
	})

	if err == nil {
		t.Fatalf("%s(inputs=%s): got error nil, want context error", fn, inputs)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("%s(inputs=%s): got error %v, want context.DeadlineExceeded or context.Canceled", fn, inputs, err)
	}

	close(blockerDone)
	time.Sleep(20 * time.Millisecond)

	if got, want := gov.QueueDepth(), 0; got != want {
		t.Fatalf("%s(inputs=%s): got QueueDepth=%d, want %d", fn, inputs, got, want)
	}
	if got, want := gov.ActiveCount(), 0; got != want {
		t.Fatalf("%s(inputs=%s): got ActiveCount=%d, want %d", fn, inputs, got, want)
	}
}

func TestGovernor_SlidingWindowLimiting(t *testing.T) {
	fn := "Governor.ExecuteWithTokens"
	window := 100 * time.Millisecond
	limit := 4
	gov := NewGovernorWithWindow(10, limit, window)

	start := time.Now()
	for i := 0; i < limit; i++ {
		err := gov.ExecuteWithTokens(context.Background(), 1, func(ctx context.Context) error {
			return nil
		})
		if err != nil {
			t.Fatalf("%s(token=1, i=%d): got error %v, want nil", fn, i, err)
		}
	}
	elapsedInitial := time.Since(start)
	if elapsedInitial >= window {
		t.Fatalf("%s(burst): got elapsed %v, want < %v", fn, elapsedInitial, window)
	}

	// 第 5 个 token 需等待窗口滑动
	err := gov.ExecuteWithTokens(context.Background(), 1, func(ctx context.Context) error {
		return nil
	})
	if err != nil {
		t.Fatalf("%s(throttled): got error %v, want nil", fn, err)
	}
	elapsedTotal := time.Since(start)
	if elapsedTotal < 80*time.Millisecond {
		t.Fatalf("%s(throttled): got total elapsed %v, want >= 80ms", fn, elapsedTotal)
	}
}

func TestGovernor_StatsObservability(t *testing.T) {
	fn := "Governor.Stats"
	gov := NewGovernor(5, 50000)
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = gov.ExecuteSubagent(context.Background(), func(ctx context.Context) error {
				_ = gov.Stats()
				time.Sleep(5 * time.Millisecond)
				return nil
			})
		}()
	}

	wg.Wait()

	stats := gov.Stats()
	if got, want := stats.ActiveCount, 0; got != want {
		t.Errorf("%s(): got ActiveCount=%d, want %d", fn, got, want)
	}
	if got, want := stats.QueueDepth, 0; got != want {
		t.Errorf("%s(): got QueueDepth=%d, want %d", fn, got, want)
	}
	if got, want := stats.TotalDispatched, int64(20); got != want {
		t.Errorf("%s(): got TotalDispatched=%d, want %d", fn, got, want)
	}
}

func TestSlidingWindowLimiter_ExceedsLimit(t *testing.T) {
	fn := "SlidingWindowLimiter.Wait"
	limiter := NewSlidingWindowLimiter(10, time.Second)
	inputs := 11
	err := limiter.Wait(context.Background(), inputs)
	if err == nil {
		t.Fatalf("%s(tokens=%d): got error nil, want error", fn, inputs)
	}
}
