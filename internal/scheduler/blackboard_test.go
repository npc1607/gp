package scheduler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TC-BB-01: Blackboard键订阅与通配符分发
func TestBlackboard_TC_BB_01_WatchAndWildcardDispatch(t *testing.T) {
	fn := "Blackboard.Watch"
	bb := NewBlackboard()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inputs := map[string]string{
		"targetKey": "k1",
		"wildcard":  "*",
		"setValue":  "v1",
	}

	keyCh := bb.Watch(ctx, "k1")
	wildcardCh := bb.Watch(ctx, "*")

	bb.Set("k1", "v1")

	select {
	case msg := <-keyCh:
		if got, want := msg, "v1"; got != want {
			t.Fatalf("%s(key=k1): got %v, want %v", fn, got, want)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("%s(key=k1, inputs=%+v): timed out waiting for key subscriber", fn, inputs)
	}

	select {
	case msg := <-wildcardCh:
		if got, want := msg, "v1"; got != want {
			t.Fatalf("%s(wildcard=*): got %v, want %v", fn, got, want)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("%s(wildcard=*, inputs=%+v): timed out waiting for wildcard subscriber", fn, inputs)
	}
}

// TC-BB-02: Blackboard订阅生命周期注销
func TestBlackboard_TC_BB_02_WatchContextCancelTeardown(t *testing.T) {
	fn := "Blackboard.Watch"
	bb := NewBlackboard()
	ctx, cancel := context.WithCancel(context.Background())

	watchKey := "k1"
	inputs := map[string]string{"key": watchKey}
	ch := bb.Watch(ctx, watchKey)

	// 确认订阅成功注册
	bb.mu.RLock()
	subCount := len(bb.subscribers[watchKey])
	bb.mu.RUnlock()
	if got, want := subCount, 1; got != want {
		t.Fatalf("%s(inputs=%+v): got subscriber count %d, want %d", fn, inputs, got, want)
	}

	// 触发取消订阅
	cancel()

	// subscriber channel 应安全关闭退出
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("%s(inputs=%+v): got open channel, want closed channel", fn, inputs)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("%s(inputs=%+v): timed out waiting for subscriber channel close", fn, inputs)
	}

	// 等待并断言 subscribers 映射清除释放
	time.Sleep(30 * time.Millisecond)
	bb.mu.RLock()
	_, exists := bb.subscribers[watchKey]
	bb.mu.RUnlock()
	if got, want := exists, false; got != want {
		t.Fatalf("%s(inputs=%+v): got subscriber key existence %v, want %v (cleaned up)", fn, inputs, got, want)
	}
}

// TC-BB-03: Blackboard快照隔离性
func TestBlackboard_TC_BB_03_SnapshotIsolation(t *testing.T) {
	fn := "Blackboard.Snapshot"
	bb := NewBlackboard()
	bb.Set("a", "v1")

	inputs := map[string]any{"initial_a": "v1"}
	snap := bb.Snapshot()

	if got, want := snap["a"], "v1"; got != want {
		t.Fatalf("%s(inputs=%+v): got snapshot['a']=%v, want %v", fn, inputs, got, want)
	}

	// 修改快照数据
	snap["a"] = "v2"
	snap["b"] = "v3"

	// 原 Blackboard 不受影响
	valA, okA := bb.Get("a")
	if got, want := okA, true; got != want {
		t.Fatalf("%s(inputs=%+v): got Has('a')=%v, want %v", fn, inputs, got, want)
	}
	if got, want := valA, "v1"; got != want {
		t.Fatalf("%s(inputs=%+v): got bb.Get('a')=%v, want %v", fn, inputs, got, want)
	}

	if got, want := bb.Has("b"), false; got != want {
		t.Fatalf("%s(inputs=%+v): got bb.Has('b')=%v, want %v", fn, inputs, got, want)
	}
}

// 上下文依赖组装表驱动规格 (BuildInputForTask)
func TestBlackboard_BuildInputForTask_TableDriven(t *testing.T) {
	fn := "Blackboard.BuildInputForTask"

	cases := []struct {
		name         string
		bbInitial    map[string]any
		dependsOn    []string
		wantContains []string
		wantExcludes []string
		wantExact    string
	}{
		{
			name: "full_deps_exist",
			bbInitial: map[string]any{
				"node1": "data1",
			},
			dependsOn: []string{"node1"},
			wantContains: []string{
				"--- 来自依赖节点 [node1] 的事实数据 ---",
				"data1",
			},
		},
		{
			name: "partial_deps_missing",
			bbInitial: map[string]any{
				"node1": "data1",
			},
			dependsOn: []string{"node1", "node2"},
			wantContains: []string{
				"--- 来自依赖节点 [node1] 的事实数据 ---",
				"data1",
			},
			wantExcludes: []string{
				"[node2]",
			},
		},
		{
			name: "empty_deps",
			bbInitial: map[string]any{
				"node1": "data1",
				"node2": "data2",
			},
			dependsOn: []string{},
			wantExact: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bb := NewBlackboard()
			for k, v := range tc.bbInitial {
				bb.Set(k, v)
			}

			got := bb.BuildInputForTask(tc.dependsOn)

			if tc.wantExact != "" || len(tc.dependsOn) == 0 {
				if got != tc.wantExact {
					t.Fatalf("%s(case=%s, deps=%v): got %q, want exact %q", fn, tc.name, tc.dependsOn, got, tc.wantExact)
				}
				return
			}

			for _, sub := range tc.wantContains {
				if !strings.Contains(got, sub) {
					t.Fatalf("%s(case=%s, deps=%v): got %q, want containing %q", fn, tc.name, tc.dependsOn, got, sub)
				}
			}

			for _, sub := range tc.wantExcludes {
				if strings.Contains(got, sub) {
					t.Fatalf("%s(case=%s, deps=%v): got %q, want excluding %q", fn, tc.name, tc.dependsOn, got, sub)
				}
			}
		})
	}
}

func TestBlackboard_CRUDAndConcurrency(t *testing.T) {
	fn := "Blackboard.CRUDAndConcurrency"
	bb := NewBlackboard()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("key_%d", id)
			bb.Set(key, id)
			if !bb.Has(key) {
				t.Errorf("%s: expected blackboard to have %s", fn, key)
			}
			val, ok := bb.Get(key)
			if !ok || val != id {
				t.Errorf("%s: Get(%s) = (%v, %v), want (%v, true)", fn, key, val, ok, id)
			}
		}(i)
	}

	wg.Wait()

	keys := bb.Keys()
	if got, want := len(keys), 50; got != want {
		t.Fatalf("%s: got keys len=%d, want %d", fn, got, want)
	}

	for i := 0; i < 50; i += 2 {
		bb.Delete(fmt.Sprintf("key_%d", i))
	}

	if got, want := len(bb.Keys()), 25; got != want {
		t.Fatalf("%s: got keys len after delete=%d, want %d", fn, got, want)
	}
}
