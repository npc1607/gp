package task_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"gp/internal/core/storage/sqlite"
	"gp/internal/core/task"
	"gp/internal/tools"
)

// TC-TASK-01: 所有权树自底向上级联取消
func TestOwnershipTree_TC_TASK_01_BottomUpCascadeAbort(t *testing.T) {
	fn := "OwnershipTree.Abort"
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "tc_task_01.db")

	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open(path=%q): got error %v, want nil", dbPath, err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	tree := task.NewOwnershipTree(store)
	ctx := context.Background()

	// 层次结构:
	// Session (root)
	//   -> Generation (child)
	//        -> Tool (child)
	//             -> Subagent (deep leaf)
	var cancelOrder []string

	sessCancel := func() { cancelOrder = append(cancelOrder, "session") }
	genCancel := func() { cancelOrder = append(cancelOrder, "generation") }
	toolCancel := func() { cancelOrder = append(cancelOrder, "tool") }
	subagentCancel := func() { cancelOrder = append(cancelOrder, "subagent") }

	tasks := []*struct {
		task   *task.Task
		cancel func()
	}{
		{
			task: &task.Task{
				ID:        "t_sess",
				SessionID: "s1",
				Type:      task.TaskTypeSession,
				Status:    task.TaskStatusRunning,
			},
			cancel: sessCancel,
		},
		{
			task: &task.Task{
				ID:        "t_gen",
				SessionID: "s1",
				ParentID:  "t_sess",
				Type:      task.TaskTypeGeneration,
				Status:    task.TaskStatusRunning,
			},
			cancel: genCancel,
		},
		{
			task: &task.Task{
				ID:        "t_tool",
				SessionID: "s1",
				ParentID:  "t_gen",
				Type:      task.TaskTypeTool,
				Status:    task.TaskStatusRunning,
			},
			cancel: toolCancel,
		},
		{
			task: &task.Task{
				ID:        "t_subagent",
				SessionID: "s1",
				ParentID:  "t_tool",
				Type:      task.TaskTypeSubagent,
				Status:    task.TaskStatusRunning,
			},
			cancel: subagentCancel,
		},
	}

	for _, item := range tasks {
		if err := tree.RegisterTask(ctx, item.task, item.cancel); err != nil {
			t.Fatalf("tree.RegisterTask(id=%s): got error %v, want nil", item.task.ID, err)
		}
	}

	// 触发根任务 Abort
	if err := tree.Abort(ctx, "t_sess"); err != nil {
		t.Fatalf("%s(taskID=%q): got error %v, want nil", fn, "t_sess", err)
	}

	// 1. 验证取消顺序严格为从底向上: subagent -> tool -> generation -> session
	expectedOrder := []string{"subagent", "tool", "generation", "session"}
	if got, want := len(cancelOrder), len(expectedOrder); got != want {
		t.Fatalf("%s: cancelOrder length got %d, want %d (order=%v)", fn, got, want, cancelOrder)
	}
	for i, wantName := range expectedOrder {
		if gotName := cancelOrder[i]; gotName != wantName {
			t.Fatalf("%s: cancelOrder[%d] got %q, want %q (full order: %v)", fn, i, gotName, wantName, cancelOrder)
		}
	}

	// 2. 验证内存中任务状态跃迁为 aborted
	allIDs := []string{"t_sess", "t_gen", "t_tool", "t_subagent"}
	for _, id := range allIDs {
		tObj, ok := tree.GetTask(id)
		if !ok || tObj == nil {
			t.Fatalf("%s: tree.GetTask(id=%q) got ok=%v tObj=%v, want ok=true", fn, id, ok, tObj)
		}
		if got, want := tObj.Status, task.TaskStatusAborted; got != want {
			t.Fatalf("%s: task %q status got %q, want %q", fn, id, got, want)
		}

		// 3. 验证持久化层 checkpoint 状态亦落盘为 aborted
		cp, err := store.GetCheckpoint(id)
		if err != nil || cp == nil {
			t.Fatalf("%s: store.GetCheckpoint(id=%q) got cp=%v err=%v, want cp!=nil err=nil", fn, id, cp, err)
		}
		if got, want := cp.Status, string(task.TaskStatusAborted); got != want {
			t.Fatalf("%s: storage checkpoint %q status got %q, want %q", fn, id, got, want)
		}
	}
}

func TestOwnershipTree_RegisterAndUpdate(t *testing.T) {
	fn := "OwnershipTree.RegisterAndUpdate"
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "task_reg.db")

	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open failed: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})

	tree := task.NewOwnershipTree(store)
	ctx := context.Background()

	sessTask := &task.Task{
		ID:        "task_sess_1",
		SessionID: "s1",
		Type:      task.TaskTypeSession,
		Status:    task.TaskStatusRunning,
	}
	if err := tree.RegisterTask(ctx, sessTask, nil); err != nil {
		t.Fatalf("%s: RegisterTask session failed: %v", fn, err)
	}

	got, ok := tree.GetTask("task_sess_1")
	if !ok || got.Status != task.TaskStatusRunning {
		t.Fatalf("%s: GetTask got status %v, want %v", fn, got.Status, task.TaskStatusRunning)
	}

	// Update task status
	if err := tree.UpdateTask(ctx, "task_sess_1", task.TaskStatusCompleted, []byte(`{"result":"done"}`)); err != nil {
		t.Fatalf("%s: UpdateTask failed: %v", fn, err)
	}

	gotAfter, _ := tree.GetTask("task_sess_1")
	if gotAfter.Status != task.TaskStatusCompleted {
		t.Fatalf("%s: GetTask after update got status %s, want %s", fn, gotAfter.Status, task.TaskStatusCompleted)
	}

	// Verify storage checkpoint is synced
	cp, err := store.GetCheckpoint("task_sess_1")
	if err != nil || cp == nil {
		t.Fatalf("%s: store.GetCheckpoint failed: %v", fn, err)
	}
	if cp.Status != string(task.TaskStatusCompleted) {
		t.Fatalf("%s: storage checkpoint status got %s, want %s", fn, cp.Status, task.TaskStatusCompleted)
	}
}

func TestReplayResolution(t *testing.T) {
	fn := "task.ResolveReplayAction"

	// 1. Tool task with ReplaySafe
	safeData, _ := json.Marshal(task.ToolCheckpoint{
		CallID:     "c1",
		ToolName:   "read",
		Arguments:  json.RawMessage(`{"path":"main.go"}`),
		ReplayMode: tools.ReplaySafe,
		Turn:       1,
	})
	safeTask := &task.Task{
		ID:             "t_read",
		Type:           task.TaskTypeTool,
		CheckpointData: safeData,
	}

	action, cp, err := task.ResolveReplayAction(safeTask)
	if err != nil || action != task.ActionSafeRerun {
		t.Fatalf("%s(safeTask): got action %s err %v, want %s nil", fn, action, err, task.ActionSafeRerun)
	}
	if got, want := cp.ToolName, "read"; got != want {
		t.Fatalf("%s(safeTask): got tool_name %q, want %q", fn, got, want)
	}

	// 2. Tool task with ReplayInterrupt
	unsafeData, _ := json.Marshal(task.ToolCheckpoint{
		CallID:     "c2",
		ToolName:   "bash",
		Arguments:  json.RawMessage(`{"command":"rm -rf /"}`),
		ReplayMode: tools.ReplayInterrupt,
		Turn:       2,
	})
	unsafeTask := &task.Task{
		ID:             "t_bash",
		Type:           task.TaskTypeTool,
		CheckpointData: unsafeData,
	}

	action2, _, err2 := task.ResolveReplayAction(unsafeTask)
	if err2 != nil || action2 != task.ActionInterruptReported {
		t.Fatalf("%s(unsafeTask): got action %s err %v, want %s nil", fn, action2, err2, task.ActionInterruptReported)
	}
}
