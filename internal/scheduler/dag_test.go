package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TC-DAG-01: DAG环路拓扑检测
func TestDAGScheduler_TC_DAG_01_CycleDetection(t *testing.T) {
	fn := "DAGScheduler.Validate"
	sched := NewDAGScheduler(nil, nil)

	nodeA := &TaskNode{ID: "A", AgentRole: "scout", DependsOn: []string{"C"}}
	nodeB := &TaskNode{ID: "B", AgentRole: "planner", DependsOn: []string{"A"}}
	nodeC := &TaskNode{ID: "C", AgentRole: "implementer", DependsOn: []string{"B"}}

	_ = sched.AddNode(nodeA)
	_ = sched.AddNode(nodeB)
	_ = sched.AddNode(nodeC)

	inputs := "cycle: A(C)->B(A)->C(B)"
	valErr := sched.Validate()
	if !errors.Is(valErr, ErrCyclicDependency) {
		t.Fatalf("%s(inputs=%s): got error %v, want %v", fn, inputs, valErr, ErrCyclicDependency)
	}

	runFn := "DAGScheduler.Run"
	runErr := sched.Run(context.Background())
	if !errors.Is(runErr, ErrCyclicDependency) {
		t.Fatalf("%s(inputs=%s): got error %v, want %v", runFn, inputs, runErr, ErrCyclicDependency)
	}
}

// TC-DAG-02: DAG拓扑Fan-out/Fan-in执行
func TestDAGScheduler_TC_DAG_02_FanOutFanIn(t *testing.T) {
	fn := "DAGScheduler.Run"
	gov := NewGovernor(10, 100000)
	bb := NewBlackboard()
	sched := NewDAGScheduler(gov, bb)

	var activeBranch int64
	var maxActiveBranch int64

	rootNode := &TaskNode{
		ID:        "root",
		AgentRole: "scout",
		CustomRunner: func(ctx context.Context, node *TaskNode, mergedInput string) (string, error) {
			return "root_manifest_data", nil
		},
	}

	nodeA := &TaskNode{
		ID:        "branch_a",
		AgentRole: "scout",
		DependsOn: []string{"root"},
		CustomRunner: func(ctx context.Context, node *TaskNode, mergedInput string) (string, error) {
			curr := atomic.AddInt64(&activeBranch, 1)
			for {
				max := atomic.LoadInt64(&maxActiveBranch)
				if curr > max {
					if atomic.CompareAndSwapInt64(&maxActiveBranch, max, curr) {
						break
					}
				} else {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			atomic.AddInt64(&activeBranch, -1)
			return "data_from_branch_a", nil
		},
	}

	nodeB := &TaskNode{
		ID:        "branch_b",
		AgentRole: "scout",
		DependsOn: []string{"root"},
		CustomRunner: func(ctx context.Context, node *TaskNode, mergedInput string) (string, error) {
			curr := atomic.AddInt64(&activeBranch, 1)
			for {
				max := atomic.LoadInt64(&maxActiveBranch)
				if curr > max {
					if atomic.CompareAndSwapInt64(&maxActiveBranch, max, curr) {
						break
					}
				} else {
					break
				}
			}
			time.Sleep(30 * time.Millisecond)
			atomic.AddInt64(&activeBranch, -1)
			return "data_from_branch_b", nil
		},
	}

	collector := &TaskNode{
		ID:        "collector",
		AgentRole: "planner",
		DependsOn: []string{"branch_a", "branch_b"},
		CustomRunner: func(ctx context.Context, node *TaskNode, mergedInput string) (string, error) {
			if !strings.Contains(mergedInput, "data_from_branch_a") || !strings.Contains(mergedInput, "data_from_branch_b") {
				return "", fmt.Errorf("collector missing branch outputs: %s", mergedInput)
			}
			return "collected_summary_output", nil
		},
	}

	_ = sched.AddNode(rootNode)
	_ = sched.AddNode(nodeA)
	_ = sched.AddNode(nodeB)
	_ = sched.AddNode(collector)

	inputs := "topology: root -> (branch_a, branch_b) -> collector"
	err := sched.Run(context.Background())
	if err != nil {
		t.Fatalf("%s(inputs=%s): got unexpected run error %v, want nil", fn, inputs, err)
	}

	nodes := []*TaskNode{rootNode, nodeA, nodeB, collector}
	for _, n := range nodes {
		if got, want := n.GetStatus(), TaskNodeCompleted; got != want {
			t.Fatalf("%s(node=%s): got status %v, want %v", fn, n.ID, got, want)
		}
		if n.GetOutput() == "" {
			t.Fatalf("%s(node=%s): got empty output, want non-empty", fn, n.ID)
		}
		if !bb.Has(n.ID) {
			t.Fatalf("%s(node=%s): blackboard missing output for node", fn, n.ID)
		}
	}

	if got, want := collector.GetOutput(), "collected_summary_output"; got != want {
		t.Fatalf("%s(collector.GetOutput): got %q, want %q", fn, got, want)
	}
}

// TC-DAG-03: DAG上游故障快速阻断下发
func TestDAGScheduler_TC_DAG_03_UpstreamFailurePropagation(t *testing.T) {
	fn := "DAGScheduler.Run"
	sched := NewDAGScheduler(nil, nil)

	customErr := errors.New("upstream failed to read file")
	nodeA := &TaskNode{
		ID:        "node_a",
		AgentRole: "scout",
		CustomRunner: func(ctx context.Context, node *TaskNode, in string) (string, error) {
			return "", customErr
		},
	}

	var downstreamExecuted bool
	nodeB := &TaskNode{
		ID:        "node_b",
		AgentRole: "planner",
		DependsOn: []string{"node_a"},
		CustomRunner: func(ctx context.Context, node *TaskNode, in string) (string, error) {
			downstreamExecuted = true
			return "output", nil
		},
	}

	_ = sched.AddNode(nodeA)
	_ = sched.AddNode(nodeB)

	inputs := "topology: node_a(fails) -> node_b"
	err := sched.Run(context.Background())
	if err == nil {
		t.Fatalf("%s(inputs=%s): got error nil, want error from upstream failure", fn, inputs)
	}

	if got, want := downstreamExecuted, false; got != want {
		t.Fatalf("%s(node_b execution): got %v, want %v (downstream runner must not execute)", fn, got, want)
	}

	if got, want := nodeA.GetStatus(), TaskNodeFailed; got != want {
		t.Errorf("%s(node_a status): got %v, want %v", fn, got, want)
	}
	if got, want := nodeB.GetStatus(), TaskNodeFailed; got != want {
		t.Errorf("%s(node_b status): got %v, want %v", fn, got, want)
	}
	if !errors.Is(nodeB.GetError(), ErrDependencyFailed) {
		t.Errorf("%s(node_b error): got %v, want %v", fn, nodeB.GetError(), ErrDependencyFailed)
	}
}

// TC-DAG-04: DAG自愈回路首轮修复闭环
func TestDAGScheduler_TC_DAG_04_RepairLoopSuccess(t *testing.T) {
	fn := "DAGScheduler.Run"
	gov := NewGovernor(5, 100000)
	bb := NewBlackboard()
	sched := NewDAGScheduler(gov, bb)

	var implementerCalls int32
	implementer := &TaskNode{
		ID:         "impl",
		AgentRole:  "implementer",
		TaskPrompt: "write math function",
		CustomRunner: func(ctx context.Context, node *TaskNode, in string) (string, error) {
			call := atomic.AddInt32(&implementerCalls, 1)
			if call == 1 {
				return "def add(a, b): return a - b", nil // buggy
			}
			return "def add(a, b): return a + b", nil // fixed
		},
	}

	var verifierRuns int32
	verifier := &TaskNode{
		ID:         "verif",
		AgentRole:  "verifier",
		DependsOn:  []string{"impl"},
		TaskPrompt: "test math function",
		CustomRunner: func(ctx context.Context, node *TaskNode, in string) (string, error) {
			run := atomic.AddInt32(&verifierRuns, 1)
			if run == 1 {
				return "", errors.New("assertion failed: add(2, 2) == 0, expected 4")
			}
			return "all tests passed: 100%", nil
		},
	}

	_ = sched.AddNode(implementer)
	_ = sched.AddNode(verifier)

	inputs := "topology: implementer -> verifier (repair 1 round)"
	err := sched.Run(context.Background())
	if err != nil {
		t.Fatalf("%s(inputs=%s): got unexpected run error %v, want nil", fn, inputs, err)
	}

	if got, want := atomic.LoadInt32(&implementerCalls), int32(2); got != want {
		t.Errorf("%s(implementerCalls): got %d, want %d", fn, got, want)
	}
	if got, want := atomic.LoadInt32(&verifierRuns), int32(2); got != want {
		t.Errorf("%s(verifierRuns): got %d, want %d", fn, got, want)
	}
	if got, want := verifier.Attempt, 1; got != want {
		t.Errorf("%s(verifier.Attempt): got %d, want %d", fn, got, want)
	}
	if got, want := verifier.GetStatus(), TaskNodeCompleted; got != want {
		t.Errorf("%s(verifier.GetStatus): got %v, want %v", fn, got, want)
	}
	if got, want := implementer.GetStatus(), TaskNodeCompleted; got != want {
		t.Errorf("%s(implementer.GetStatus): got %v, want %v", fn, got, want)
	}
}

// TC-DAG-05: DAG自愈两轮熔断硬中断
func TestDAGScheduler_TC_DAG_05_CircuitBreakerTrip(t *testing.T) {
	fn := "DAGScheduler.Run"
	gov := NewGovernor(5, 100000)
	bb := NewBlackboard()
	sched := NewDAGScheduler(gov, bb)
	sched.SetMaxRepairAttempts(2)

	implementer := &TaskNode{
		ID:         "impl",
		AgentRole:  "implementer",
		TaskPrompt: "write math function",
		CustomRunner: func(ctx context.Context, node *TaskNode, in string) (string, error) {
			return "broken code", nil
		},
	}

	var verifierRuns int32
	verifier := &TaskNode{
		ID:         "verif",
		AgentRole:  "verifier",
		DependsOn:  []string{"impl"},
		TaskPrompt: "test math function",
		CustomRunner: func(ctx context.Context, node *TaskNode, in string) (string, error) {
			atomic.AddInt32(&verifierRuns, 1)
			return "", errors.New("persistent test failure: syntax error")
		},
	}

	_ = sched.AddNode(implementer)
	_ = sched.AddNode(verifier)

	inputs := "topology: implementer -> verifier (persistent failure, maxAttempts=2)"
	err := sched.Run(context.Background())
	if !errors.Is(err, ErrCircuitBreakerTripped) {
		t.Fatalf("%s(inputs=%s): got error %v, want %v", fn, inputs, err, ErrCircuitBreakerTripped)
	}

	if got, want := verifier.Attempt, 2; got != want {
		t.Errorf("%s(verifier.Attempt): got %d, want %d", fn, got, want)
	}
	if got, want := verifier.GetStatus(), TaskNodeFailed; got != want {
		t.Errorf("%s(verifier.GetStatus): got %v, want %v", fn, got, want)
	}
	if !errors.Is(verifier.GetError(), ErrCircuitBreakerTripped) {
		t.Errorf("%s(verifier.GetError): got %v, want %v", fn, verifier.GetError(), ErrCircuitBreakerTripped)
	}
}

func TestDAGScheduler_LinearExecution(t *testing.T) {
	fn := "DAGScheduler.Run"
	gov := NewGovernor(10, 100000)
	bb := NewBlackboard()
	sched := NewDAGScheduler(gov, bb)

	nodes := []*TaskNode{
		{ID: "scout", AgentRole: "scout", TaskPrompt: "inspect code"},
		{ID: "planner", AgentRole: "planner", TaskPrompt: "plan changes", DependsOn: []string{"scout"}},
		{ID: "implementer", AgentRole: "implementer", TaskPrompt: "write code", DependsOn: []string{"planner"}},
		{ID: "verifier", AgentRole: "verifier", TaskPrompt: "run tests", DependsOn: []string{"implementer"}},
	}

	for _, n := range nodes {
		if err := sched.AddNode(n); err != nil {
			t.Fatalf("%s(AddNode=%s): got error %v, want nil", fn, n.ID, err)
		}
	}

	sched.SetDefaultRunner(func(ctx context.Context, node *TaskNode, mergedInput string) (string, error) {
		return fmt.Sprintf("result_of_%s", node.ID), nil
	})

	if err := sched.Run(context.Background()); err != nil {
		t.Fatalf("%s(linear): got unexpected error %v, want nil", fn, err)
	}

	for _, n := range nodes {
		if got, want := n.GetStatus(), TaskNodeCompleted; got != want {
			t.Errorf("%s(node=%s): got status %v, want %v", fn, n.ID, got, want)
		}
		expectedOut := fmt.Sprintf("result_of_%s", n.ID)
		if got, want := n.GetOutput(), expectedOut; got != want {
			t.Errorf("%s(node=%s): got output %q, want %q", fn, n.ID, got, want)
		}
		val, ok := bb.Get(n.ID)
		if !ok || val != expectedOut {
			t.Errorf("%s(bb.Get=%s): got (%v, %v), want (%q, true)", fn, n.ID, val, ok, expectedOut)
		}
	}
}
