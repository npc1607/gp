package tools_test

import (
	"encoding/json"
	"testing"

	"gp/internal/tools"
	"gp/internal/tools/builtin"
)

type mockUnregisteredTool struct{}

func (m *mockUnregisteredTool) Name() string { return "mock_unregistered" }
func (m *mockUnregisteredTool) Description() string {
	return "custom tool without replayable interface"
}
func (m *mockUnregisteredTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (m *mockUnregisteredTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	return &tools.ToolResult{Content: "done"}, nil
}

// Table-Driven Case Spec: 工具重放策略判定 (Replay Policy Resolution)
func TestReplayPolicyResolution(t *testing.T) {
	fn := "tools.GetReplayMode"
	tests := []struct {
		name       string
		input      tools.Tool
		wantOutput tools.ReplayMode
		wantErr    error
		note       string
	}{
		{
			name:       "replay_mode_read",
			input:      &builtin.ReadTool{},
			wantOutput: tools.ReplaySafe,
			wantErr:    nil,
			note:       "只读工具支持安全自动重跑",
		},
		{
			name:       "replay_mode_write",
			input:      &builtin.WriteTool{},
			wantOutput: tools.ReplayInterrupt,
			wantErr:    nil,
			note:       "写入工具中断防护，禁止重复落盘",
		},
		{
			name:       "replay_mode_edit",
			input:      &builtin.EditTool{},
			wantOutput: tools.ReplayInterrupt,
			wantErr:    nil,
			note:       "文件修改中断防护",
		},
		{
			name:       "replay_mode_bash",
			input:      &builtin.BashTool{},
			wantOutput: tools.ReplayInterrupt,
			wantErr:    nil,
			note:       "Shell命令中断防护，避免不可逆副作用",
		},
		{
			name:       "replay_mode_unregistered",
			input:      &mockUnregisteredTool{},
			wantOutput: tools.ReplayInterrupt,
			wantErr:    nil,
			note:       "缺省回退安全策略，统一中断由模型裁决",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := tools.GetReplayMode(tc.input)
			if got != tc.wantOutput {
				t.Fatalf("%s(input=%s [%s]): got %v, want %v", fn, tc.name, tc.note, got, tc.wantOutput)
			}
		})
	}
}

func TestRegistry_Operations(t *testing.T) {
	fn := "tools.Registry"
	reg := tools.NewRegistry()

	readTool := &builtin.ReadTool{}
	writeTool := &builtin.WriteTool{}

	if err := reg.Register(readTool); err != nil {
		t.Fatalf("%s.Register(read): got error %v, want nil", fn, err)
	}
	if err := reg.Register(writeTool); err != nil {
		t.Fatalf("%s.Register(write): got error %v, want nil", fn, err)
	}

	// Duplicate registration error
	if err := reg.Register(readTool); err == nil {
		t.Fatalf("%s.Register(duplicate read): got nil error, want duplicate registration error", fn)
	}

	// Get tool
	gotTool, ok := reg.Get("read")
	if !ok || gotTool == nil {
		t.Fatalf("%s.Get(name=%q): got ok=%v tool=%v, want ok=true", fn, "read", ok, gotTool)
	}
	if gotName, wantName := gotTool.Name(), "read"; gotName != wantName {
		t.Fatalf("%s.Get(name=%q).Name(): got %q, want %q", fn, "read", gotName, wantName)
	}

	// List
	list := reg.List()
	if gotLen, wantLen := len(list), 2; gotLen != wantLen {
		t.Fatalf("%s.List(): got length %d, want %d", fn, gotLen, wantLen)
	}

	// Scoped registry
	scoped := reg.ScopedRegistry([]string{"read"})
	if _, found := scoped.Get("read"); !found {
		t.Fatalf("%s.ScopedRegistry: got not found for 'read', want found", fn)
	}
	if _, found := scoped.Get("write"); found {
		t.Fatalf("%s.ScopedRegistry: got found for 'write', want not found", fn)
	}
}
