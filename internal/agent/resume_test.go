package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gp/internal/ai"
	"gp/internal/core/session"
	"gp/internal/core/storage/sqlite"
	"gp/internal/core/task"
	"gp/internal/tools"
)

type mockCountingTool struct {
	name       string
	replayMode tools.ReplayMode
	execCount  atomic.Int32
}

func (m *mockCountingTool) Name() string                 { return m.name }
func (m *mockCountingTool) Description() string          { return "counting tool" }
func (m *mockCountingTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (m *mockCountingTool) ReplayMode() tools.ReplayMode { return m.replayMode }
func (m *mockCountingTool) Execute(ctx *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	m.execCount.Add(1)
	return &tools.ToolResult{Content: fmt.Sprintf("%s executed count=%d", m.name, m.execCount.Load())}, nil
}

// TC-RES-01: 断点续跑-安全工具自动重跑
func TestResume_TC_RES_01_SafeToolReplay(t *testing.T) {
	fn := "agent.Resume.TC_RES_01"
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "safe_replay.db")
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open(path=%q): got error %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	sessID := "sess_safe_1"
	st, err := session.NewSessionTree(sessID, store)
	if err != nil {
		t.Fatalf("session.NewSessionTree(sessID=%q): got error %v, want nil", sessID, err)
	}

	callID := "call_safe_99"
	// 模拟崩溃现场：大模型发出了 read 工具调用
	st.AppendMessage(ai.Message{
		Role:    ai.RoleUser,
		Content: "Please read file",
	})
	callIdx := 0
	st.AppendMessage(ai.Message{
		Role: ai.RoleAssistant,
		ToolCalls: []ai.ToolCall{
			{
				Index: &callIdx,
				ID:    callID,
				Type:  "function",
				Function: ai.FunctionCall{
					Name:      "mock_read",
					Arguments: json.RawMessage(`{"path":"test.txt"}`),
				},
			},
		},
	})

	// 模拟在工具执行过程中进程崩溃，task_checkpoint 停留在 running 状态
	tree := task.NewOwnershipTree(store)
	cpData, _ := json.Marshal(task.ToolCheckpoint{
		CallID:     callID,
		ToolName:   "mock_read",
		Arguments:  json.RawMessage(`{"path":"test.txt"}`),
		ReplayMode: tools.ReplaySafe,
		Turn:       0,
	})
	_ = tree.RegisterTask(context.Background(), &task.Task{
		ID:             fmt.Sprintf("task_tool_%s", callID),
		SessionID:      sessID,
		Type:           task.TaskTypeTool,
		Status:         task.TaskStatusRunning,
		ReplayMode:     tools.ReplaySafe,
		CheckpointData: cpData,
	}, nil)

	mockTool := &mockCountingTool{
		name:       "mock_read",
		replayMode: tools.ReplaySafe,
	}
	reg := tools.NewRegistry()
	_ = reg.Register(mockTool)

	mockProv := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			// 断点续跑后，模型看到工具执行结果并输出最终结论
			func(req *ai.ChatRequest) []ai.StreamChunk {
				return []ai.StreamChunk{
					{DeltaText: "File read successfully after recovery."},
				}
			},
		},
	}

	res, err := Resume(context.Background(), ResumeOptions{
		Storage:   store,
		Provider:  mockProv,
		Registry:  reg,
		SessionID: sessID,
	})
	if err != nil {
		t.Fatalf("%s(opts): got error %v, want nil", fn, err)
	}

	// 1. 校验安全工具是否成功重跑 (执行次数+1)
	if got, want := mockTool.execCount.Load(), int32(1); got != want {
		t.Fatalf("%s: tool execution count got %d, want %d", fn, got, want)
	}

	// 2. 校验 ReplayAction 为 safe_rerun
	if got, want := res.ReplayAction, string(task.ActionSafeRerun); got != want {
		t.Fatalf("%s: res.ReplayAction got %q, want %q", fn, got, want)
	}

	// 3. 校验最终结论响应
	wantFinalResp := "File read successfully after recovery."
	if got, want := res.FinalResponse, wantFinalResp; got != want {
		t.Fatalf("%s: res.FinalResponse got %q, want %q", fn, got, want)
	}

	// 4. 校验持久层状态是否跃迁为 completed
	cpAfter, err := store.GetCheckpoint(fmt.Sprintf("task_tool_%s", callID))
	if err != nil || cpAfter == nil {
		t.Fatalf("%s: store.GetCheckpoint(id=%q) got cp=%v err=%v, want non-nil cp err=nil", fn, callID, cpAfter, err)
	}
	if got, want := cpAfter.Status, string(task.TaskStatusCompleted); got != want {
		t.Fatalf("%s: checkpoint status got %q, want %q", fn, got, want)
	}
}

// TC-RES-02: 断点续跑-副作用工具中断拦截
func TestResume_TC_RES_02_UnsafeToolInterrupt(t *testing.T) {
	fn := "agent.Resume.TC_RES_02"
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "unsafe_replay.db")
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open(path=%q): got error %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	sessID := "sess_unsafe_1"
	st, err := session.NewSessionTree(sessID, store)
	if err != nil {
		t.Fatalf("session.NewSessionTree(sessID=%q): got error %v, want nil", sessID, err)
	}

	callID := "call_bash_88"
	st.AppendMessage(ai.Message{
		Role:    ai.RoleUser,
		Content: "Please execute command",
	})
	callIdx := 0
	st.AppendMessage(ai.Message{
		Role: ai.RoleAssistant,
		ToolCalls: []ai.ToolCall{
			{
				Index: &callIdx,
				ID:    callID,
				Type:  "function",
				Function: ai.FunctionCall{
					Name:      "mock_bash",
					Arguments: json.RawMessage(`{"command":"deploy.sh"}`),
				},
			},
		},
	})

	// 模拟在有副作用工具执行中被强杀，检查点停留在 running
	tree := task.NewOwnershipTree(store)
	cpData, _ := json.Marshal(task.ToolCheckpoint{
		CallID:     callID,
		ToolName:   "mock_bash",
		Arguments:  json.RawMessage(`{"command":"deploy.sh"}`),
		ReplayMode: tools.ReplayInterrupt,
		Turn:       0,
	})
	_ = tree.RegisterTask(context.Background(), &task.Task{
		ID:             fmt.Sprintf("task_tool_%s", callID),
		SessionID:      sessID,
		Type:           task.TaskTypeTool,
		Status:         task.TaskStatusRunning,
		ReplayMode:     tools.ReplayInterrupt,
		CheckpointData: cpData,
	}, nil)

	mockTool := &mockCountingTool{
		name:       "mock_bash",
		replayMode: tools.ReplayInterrupt,
	}
	reg := tools.NewRegistry()
	_ = reg.Register(mockTool)

	var receivedPromptWithNotice bool
	mockProv := &mockProvider{
		chatHandlers: []func(req *ai.ChatRequest) []ai.StreamChunk{
			func(req *ai.ChatRequest) []ai.StreamChunk {
				// 验证最后一条消息包含了中断通知
				lastMsg := req.Messages[len(req.Messages)-1]
				if lastMsg.Role == ai.RoleTool && lastMsg.ToolCallID == callID {
					receivedPromptWithNotice = true
				}
				return []ai.StreamChunk{
					{DeltaText: "I noticed the bash execution was interrupted. I will check the status."},
				}
			},
		},
	}

	res, err := Resume(context.Background(), ResumeOptions{
		Storage:   store,
		Provider:  mockProv,
		Registry:  reg,
		SessionID: sessID,
	})
	if err != nil {
		t.Fatalf("%s(opts): got error %v, want nil", fn, err)
	}

	// 1. 校验有副作用工具绝对未被二次重复执行！
	if got, want := mockTool.execCount.Load(), int32(0); got != want {
		t.Fatalf("%s: tool execution count got %d, want %d (should NOT rerun)", fn, got, want)
	}

	// 2. 校验 ReplayAction 为 interrupted_reported
	if got, want := res.ReplayAction, string(task.ActionInterruptReported); got != want {
		t.Fatalf("%s: res.ReplayAction got %q, want %q", fn, got, want)
	}

	// 3. 校验大模型收到了合成的中断通知
	if got, want := receivedPromptWithNotice, true; got != want {
		t.Fatalf("%s: receivedPromptWithNotice got %v, want %v", fn, got, want)
	}

	// 4. 校验持久层状态是否已变更为 interrupted
	cpAfter, err := store.GetCheckpoint(fmt.Sprintf("task_tool_%s", callID))
	if err != nil || cpAfter == nil {
		t.Fatalf("%s: store.GetCheckpoint(id=%q) got cp=%v err=%v, want non-nil cp err=nil", fn, callID, cpAfter, err)
	}
	if got, want := cpAfter.Status, string(task.TaskStatusInterrupted); got != want {
		t.Fatalf("%s: checkpoint status got %q, want %q", fn, got, want)
	}
}

// TC-RES-03: 断点续跑-无未完成任务边界
func TestResume_TC_RES_03_NoUnfinishedTask(t *testing.T) {
	fn := "agent.Resume.TC_RES_03"
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "empty.db")
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open(path=%q): got error %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	mock := &mockProvider{}
	res, err := Resume(context.Background(), ResumeOptions{
		Storage:  store,
		Provider: mock,
	})
	if got, want := err, ErrNoUnfinishedTask; got != want {
		t.Fatalf("%s: got error %v, want %v", fn, got, want)
	}
	if res != nil {
		t.Fatalf("%s: got non-nil result %v, want nil", fn, res)
	}

	// 验证存储中依然没有任何未完成任务
	unfinished, err := store.ListUnfinishedCheckpoints()
	if err != nil {
		t.Fatalf("%s: store.ListUnfinishedCheckpoints() got error %v, want nil", fn, err)
	}
	if got, want := len(unfinished), 0; got != want {
		t.Fatalf("%s: unfinished count got %d, want %d", fn, got, want)
	}
}
