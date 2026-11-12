package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gp/internal/core/session"
	"gp/internal/scheduler"
	"gp/internal/tools"
)

// TC-SUB-01: Subagent并发分发与结果聚合
func TestSubagentTool_TC_SUB_01_ConcurrentFanOutAndAggregate(t *testing.T) {
	fn := "SubagentTool.Execute"
	gov := scheduler.NewGovernor(10, 100000)

	runner := func(ctx context.Context, role, taskPrompt string, st *session.SessionTree, reg *tools.Registry) (string, error) {
		time.Sleep(10 * time.Millisecond)
		return "executed_" + taskPrompt, nil
	}

	subTool := NewSubagentTool(gov, tools.NewRegistry(), runner)
	rawArgs := []byte(`{
		"tasks": [
			{"agent": "scout", "task": "t1"},
			{"agent": "planner", "task": "t2"}
		]
	}`)

	inputs := string(rawArgs)
	res, err := subTool.Execute(&tools.ToolContext{Context: context.Background()}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%s): got unexpected error %v, want nil", fn, inputs, err)
	}

	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(inputs=%s): got IsError=%v, want %v (content=%q)", fn, inputs, got, want, res.Content)
	}

	wantSubstrings := []string{
		"[子 Agent: scout]",
		"executed_t1",
		"[子 Agent: planner]",
		"executed_t2",
	}

	for _, sub := range wantSubstrings {
		if !strings.Contains(res.Content, sub) {
			t.Errorf("%s(inputs=%s): got content %q, want containing %q", fn, inputs, res.Content, sub)
		}
	}
}

// TC-SUB-02: Subagent空任务入参校验
func TestSubagentTool_TC_SUB_02_EmptyTasksValidation(t *testing.T) {
	fn := "SubagentTool.Execute"
	tool := NewSubagentTool(nil, nil, nil)
	rawArgs := []byte(`{"tasks": []}`)
	inputs := string(rawArgs)

	res, err := tool.Execute(&tools.ToolContext{Context: context.Background()}, rawArgs)
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("%s(inputs=%s): got (res=%+v, err=%v), want error or res.IsError=true", fn, inputs, res, err)
	}

	if err != nil {
		wantSubstr := "no tasks provided"
		if !strings.Contains(err.Error(), wantSubstr) {
			t.Fatalf("%s(inputs=%s): got err %q, want containing %q", fn, inputs, err.Error(), wantSubstr)
		}
	}
}

// 角色工具权限裁剪表驱动规格 (ResolveToolsForRole)
func TestSubagentTool_ResolveToolsForRole_TableDriven(t *testing.T) {
	fn := "ResolveToolsForRole"

	cases := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "role_scout",
			input:    "scout",
			expected: []string{"read"},
		},
		{
			name:     "role_reviewer",
			input:    "reviewer",
			expected: []string{"read"},
		},
		{
			name:     "role_planner",
			input:    "planner",
			expected: []string{"read"},
		},
		{
			name:     "role_verifier",
			input:    "verifier",
			expected: []string{"read", "bash"},
		},
		{
			name:     "role_implementer",
			input:    "implementer",
			expected: []string{"read", "write", "edit", "bash", "ask_peer_agent"},
		},
		{
			name:     "role_unknown_fallback",
			input:    "custom_worker",
			expected: []string{"read"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveToolsForRole(tc.input)

			if len(got) != len(tc.expected) {
				t.Fatalf("%s(case=%s, input=%q): got len=%d (%v), want len=%d (%v)",
					fn, tc.name, tc.input, len(got), got, len(tc.expected), tc.expected)
			}

			for i := range tc.expected {
				if got[i] != tc.expected[i] {
					t.Fatalf("%s(case=%s, input=%q): got tool[%d]=%q, want %q",
						fn, tc.name, tc.input, i, got[i], tc.expected[i])
				}
			}
		})
	}
}

func TestSubagentTool_ContractAndSchema(t *testing.T) {
	fn := "SubagentTool.Contract"
	tool := NewSubagentTool(nil, nil, nil)
	if got, want := tool.Name(), "subagent"; got != want {
		t.Errorf("%s: got Name()=%q, want %q", fn, got, want)
	}
	if tool.Description() == "" {
		t.Errorf("%s: Description() is empty", fn)
	}
	if got, want := tool.ReplayMode(), tools.ReplayInterrupt; got != want {
		t.Errorf("%s: got ReplayMode()=%q, want %q", fn, got, want)
	}

	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatalf("%s: InputSchema() is invalid JSON: %v", fn, err)
	}
}

func TestSubagentTool_GovernorSlotEnforcement(t *testing.T) {
	fn := "SubagentTool.GovernorSlotEnforcement"
	gov := scheduler.NewGovernor(2, 100000)
	var currentActive int64
	var maxActive int64
	var mu sync.Mutex

	runner := func(ctx context.Context, role, taskPrompt string, st *session.SessionTree, reg *tools.Registry) (string, error) {
		mu.Lock()
		currentActive++
		if currentActive > maxActive {
			maxActive = currentActive
		}
		mu.Unlock()

		time.Sleep(25 * time.Millisecond)

		mu.Lock()
		currentActive--
		mu.Unlock()
		return "ok", nil
	}

	subTool := NewSubagentTool(gov, tools.NewRegistry(), runner)
	inputJSON := []byte(`{
		"tasks": [
			{"agent": "scout", "task": "1"},
			{"agent": "scout", "task": "2"},
			{"agent": "scout", "task": "3"},
			{"agent": "scout", "task": "4"}
		]
	}`)

	_, err := subTool.Execute(&tools.ToolContext{Context: context.Background()}, inputJSON)
	if err != nil {
		t.Fatalf("%s: got error %v, want nil", fn, err)
	}

	mu.Lock()
	observedMax := maxActive
	mu.Unlock()

	if observedMax > 2 {
		t.Errorf("%s: got max active subagents %d, want <= 2", fn, observedMax)
	}
}

func TestSubagentTool_Validation(t *testing.T) {
	fn := "SubagentTool.Validation"
	tool := NewSubagentTool(nil, nil, nil)

	// Invalid JSON
	raw := []byte(`{invalid json`)
	_, err := tool.Execute(nil, raw)
	if err == nil {
		t.Errorf("%s(inputs=%s): got error nil, want error for invalid json", fn, string(raw))
	}
}

func TestSubagentTool_ConcurrentExecution(t *testing.T) {
	fn := "SubagentTool.Execute"
	baseReg, err := NewDefaultRegistry()
	if err != nil {
		t.Fatalf("%s: NewDefaultRegistry failed: %v", fn, err)
	}

	gov := scheduler.NewGovernor(10, 100000)
	var activeSubagents int64
	var maxActive int64

	runner := func(ctx context.Context, role, taskPrompt string, st *session.SessionTree, reg *tools.Registry) (string, error) {
		curr := atomic.AddInt64(&activeSubagents, 1)
		for {
			max := atomic.LoadInt64(&maxActive)
			if curr > max {
				if atomic.CompareAndSwapInt64(&maxActive, max, curr) {
					break
				}
			} else {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&activeSubagents, -1)

		if st == nil {
			return "", errors.New("missing session tree")
		}
		if reg == nil {
			return "", errors.New("missing scoped registry")
		}

		expectedAllowed := ResolveToolsForRole(role)
		for _, toolName := range expectedAllowed {
			if _, ok := reg.Get(toolName); !ok {
				return "", errors.New("expected tool not found in scoped registry: " + toolName)
			}
		}

		return "findings for " + taskPrompt, nil
	}

	subTool := NewSubagentTool(gov, baseReg, runner)
	inputJSON := []byte(`{
		"tasks": [
			{"agent": "scout", "task": "analyze auth module"},
			{"agent": "reviewer", "task": "review security checklist"},
			{"agent": "implementer", "task": "implement token refresher"}
		]
	}`)

	res, err := subTool.Execute(&tools.ToolContext{Context: context.Background()}, inputJSON)
	if err != nil {
		t.Fatalf("%s: got error %v, want nil", fn, err)
	}
	if res.IsError {
		t.Fatalf("%s: got IsError=true, want false (content=%s)", fn, res.Content)
	}

	if !strings.Contains(res.Content, "analyze auth module") || !strings.Contains(res.Content, "review security checklist") {
		t.Errorf("%s: content missing expected task results: %s", fn, res.Content)
	}
}
