package builtin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gp/internal/tools"
)

func TestBashTool_Basic(t *testing.T) {
	fn := "BashTool.Execute"
	tool := &BashTool{}
	if got, want := tool.Name(), "bash"; got != want {
		t.Errorf("BashTool.Name(): got %q, want %q", got, want)
	}

	inputs := bashArgs{Command: "echo 'hello from bash'"}
	rawArgs, _ := json.Marshal(inputs)
	res, err := tool.Execute(&tools.ToolContext{}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected go error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v (content=%q)", fn, inputs, got, want, res.Content)
	}
	if wantSub := "hello from bash"; !strings.Contains(res.Content, wantSub) {
		t.Fatalf("%s(inputs=%+v): got output %q, want substring %q", fn, inputs, res.Content, wantSub)
	}
}

func TestBashTool_EnvAndCwd(t *testing.T) {
	fn := "BashTool.Execute"
	tmpDir := t.TempDir()
	tool := &BashTool{}

	inputs := bashArgs{Command: "pwd && echo $CUSTOM_TEST_VAR"}
	rawArgs, _ := json.Marshal(inputs)
	ctx := &tools.ToolContext{
		Cwd: tmpDir,
		Env: map[string]string{
			"CUSTOM_TEST_VAR": "magic_val_123",
		},
	}
	res, err := tool.Execute(ctx, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected go error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, false; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v (content=%q)", fn, inputs, got, want, res.Content)
	}
	if wantSub := "magic_val_123"; !strings.Contains(res.Content, wantSub) {
		t.Fatalf("%s(inputs=%+v): got output %q, want substring %q", fn, inputs, res.Content, wantSub)
	}
}

func TestBashTool_NonZeroExit(t *testing.T) {
	fn := "BashTool.Execute"
	tool := &BashTool{}
	inputs := bashArgs{Command: "exit 42"}
	rawArgs, _ := json.Marshal(inputs)
	res, err := tool.Execute(&tools.ToolContext{}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected go error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, true; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v", fn, inputs, got, want)
	}
}

func TestBashTool_Timeout(t *testing.T) {
	fn := "BashTool.Execute"
	tool := &BashTool{}
	inputs := bashArgs{
		Command: "sleep 5",
		Timeout: 0.1,
	}
	rawArgs, _ := json.Marshal(inputs)
	res, err := tool.Execute(&tools.ToolContext{}, rawArgs)
	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected go error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, true; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v", fn, inputs, got, want)
	}
	if wantSub := "timed out"; !strings.Contains(res.Content, wantSub) {
		t.Fatalf("%s(inputs=%+v): got content %q, want substring %q", fn, inputs, res.Content, wantSub)
	}
}

// Table-driven tests for BashTool
func TestBashTool_TableDriven(t *testing.T) {
	fn := "BashTool.Execute"
	tool := &BashTool{}

	cases := []struct {
		name       string
		command    string
		timeout    float64
		wantError  bool
		wantSubstr string
	}{
		{
			name:       "echo_success",
			command:    "echo 'table driven bash test'",
			wantError:  false,
			wantSubstr: "table driven bash test",
		},
		{
			name:       "non_zero_exit",
			command:    "exit 7",
			wantError:  true,
			wantSubstr: "exit",
		},
		{
			name:       "empty_command",
			command:    "   ",
			wantError:  true,
			wantSubstr: "command is required",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inputs := bashArgs{
				Command: tc.command,
				Timeout: tc.timeout,
			}
			rawArgs, _ := json.Marshal(inputs)

			res, err := tool.Execute(&tools.ToolContext{}, rawArgs)
			if err != nil {
				t.Fatalf("%s(case=%s, inputs=%+v): got unexpected go error %v, want nil", fn, tc.name, inputs, err)
			}

			if got, want := res.IsError, tc.wantError; got != want {
				t.Fatalf("%s(case=%s, inputs=%+v): got IsError=%v, want %v (content=%q)", fn, tc.name, inputs, got, want, res.Content)
			}

			if tc.wantSubstr != "" && !strings.Contains(res.Content, tc.wantSubstr) {
				t.Fatalf("%s(case=%s, inputs=%+v): got content %q, want substring %q", fn, tc.name, inputs, res.Content, tc.wantSubstr)
			}
		})
	}
}

// TC-BS-001: Bash-进程组隔离与超时强杀
func TestBashTool_TC_BS_001_ProcessGroupTimeout(t *testing.T) {
	fn := "BashTool.Execute"
	tool := &BashTool{}

	// Spawns subshell child processes to verify process group killing
	inputs := bashArgs{
		Command: "(sleep 10 & wait)",
		Timeout: 0.2,
	}
	rawArgs, _ := json.Marshal(inputs)

	start := time.Now()
	res, err := tool.Execute(&tools.ToolContext{}, rawArgs)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("%s(inputs=%+v): got unexpected go error %v, want nil", fn, inputs, err)
	}
	if got, want := res.IsError, true; got != want {
		t.Fatalf("%s(inputs=%+v): got IsError=%v, want %v (content=%q)", fn, inputs, got, want, res.Content)
	}
	if wantSub := "timed out"; !strings.Contains(res.Content, wantSub) {
		t.Fatalf("%s(inputs=%+v): got content %q, want substring %q", fn, inputs, res.Content, wantSub)
	}

	// Must be terminated promptly via process group SIGKILL, well before 10s
	if elapsed > 3*time.Second {
		t.Fatalf("%s(inputs=%+v): command took %v, want < 3s", fn, inputs, elapsed)
	}
}
