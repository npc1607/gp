package task

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gp/internal/core/storage"
)

// OwnershipTree 任务所有权级联树
// 维护 Session -> Generation -> Tool -> Subagent 树形依赖，支持从底向上级联取消
type OwnershipTree struct {
	mu          sync.RWMutex
	tasks       map[string]*Task
	children    map[string][]string // parentID -> childIDs
	cancelFuncs map[string]context.CancelFunc
	storage     storage.Storage
}

// NewOwnershipTree 创建所有权级联树
func NewOwnershipTree(store storage.Storage) *OwnershipTree {
	return &OwnershipTree{
		tasks:       make(map[string]*Task),
		children:    make(map[string][]string),
		cancelFuncs: make(map[string]context.CancelFunc),
		storage:     store,
	}
}

// RegisterTask 注册任务节点并持久化初始检查点
func (o *OwnershipTree) RegisterTask(ctx context.Context, t *Task, cancel context.CancelFunc) error {
	if t == nil || t.ID == "" {
		return fmt.Errorf("task and task ID cannot be empty")
	}

	o.mu.Lock()
	now := time.Now()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = now
	}
	t.UpdatedAt = now

	cloned := *t
	o.tasks[t.ID] = &cloned
	if cancel != nil {
		o.cancelFuncs[t.ID] = cancel
	}

	if t.ParentID != "" {
		o.children[t.ParentID] = append(o.children[t.ParentID], t.ID)
	}
	o.mu.Unlock()

	if o.storage != nil {
		err := o.storage.Commit(ctx, func(tx storage.Transaction) error {
			return tx.SaveCheckpoint(&storage.TaskCheckpoint{
				TaskID:         t.ID,
				SessionID:      t.SessionID,
				TaskType:       string(t.Type),
				Status:         string(t.Status),
				CheckpointData: t.CheckpointData,
				UpdatedAt:      t.UpdatedAt,
			})
		})
		if err != nil {
			return fmt.Errorf("failed to commit task checkpoint: %w", err)
		}
	}

	return nil
}

// UpdateTask 更新任务状态与检查点数据
func (o *OwnershipTree) UpdateTask(ctx context.Context, taskID string, status TaskStatus, data []byte) error {
	o.mu.Lock()
	t, ok := o.tasks[taskID]
	if !ok && o.storage != nil {
		cp, err := o.storage.GetCheckpoint(taskID)
		if err == nil && cp != nil {
			t = &Task{
				ID:             cp.TaskID,
				SessionID:      cp.SessionID,
				Type:           TaskType(cp.TaskType),
				Status:         TaskStatus(cp.Status),
				CheckpointData: cp.CheckpointData,
				UpdatedAt:      cp.UpdatedAt,
			}
			o.tasks[taskID] = t
			ok = true
		}
	}
	if !ok {
		o.mu.Unlock()
		return fmt.Errorf("task not found: %s", taskID)
	}
	t.Status = status
	t.UpdatedAt = time.Now()
	if data != nil {
		t.CheckpointData = data
	}
	cloned := *t
	o.mu.Unlock()

	if o.storage != nil {
		err := o.storage.Commit(ctx, func(tx storage.Transaction) error {
			return tx.SaveCheckpoint(&storage.TaskCheckpoint{
				TaskID:         cloned.ID,
				SessionID:      cloned.SessionID,
				TaskType:       string(cloned.Type),
				Status:         string(cloned.Status),
				CheckpointData: cloned.CheckpointData,
				UpdatedAt:      cloned.UpdatedAt,
			})
		})
		if err != nil {
			return fmt.Errorf("failed to update task checkpoint in storage: %w", err)
		}
	}

	return nil
}

// GetTask 获取任务拷贝
func (o *OwnershipTree) GetTask(taskID string) (*Task, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	t, ok := o.tasks[taskID]
	if !ok && o.storage != nil {
		cp, err := o.storage.GetCheckpoint(taskID)
		if err == nil && cp != nil {
			t = &Task{
				ID:             cp.TaskID,
				SessionID:      cp.SessionID,
				Type:           TaskType(cp.TaskType),
				Status:         TaskStatus(cp.Status),
				CheckpointData: cp.CheckpointData,
				UpdatedAt:      cp.UpdatedAt,
			}
			o.tasks[taskID] = t
			ok = true
		}
	}
	if !ok {
		return nil, false
	}
	cloned := *t
	return &cloned, true
}

// ListTasks 获取所有任务列表
func (o *OwnershipTree) ListTasks() []*Task {
	o.mu.RLock()
	defer o.mu.RUnlock()
	res := make([]*Task, 0, len(o.tasks))
	for _, t := range o.tasks {
		cloned := *t
		res = append(res, &cloned)
	}
	return res
}

// Abort 从底向上（Bottom-Up）级联取消 taskID 及其所有子任务
// 1. 后序遍历（Post-Order DFS）收集所有活跃子任务（深度最深的叶子任务排在前面）；
// 2. 将 taskID 追加在最后；
// 3. 逐个调用取消函数，并同步更新状态为 aborted，落盘持久化。
func (o *OwnershipTree) Abort(ctx context.Context, taskID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()

	if _, ok := o.tasks[taskID]; !ok {
		return fmt.Errorf("task not found: %s", taskID)
	}

	// 1. Post-order DFS 收集任务 ID
	var order []string
	var collect func(curr string)
	collect = func(curr string) {
		for _, childID := range o.children[curr] {
			collect(childID)
		}
		order = append(order, curr)
	}
	collect(taskID)

	// 2. 从底向上级联取消并更新状态
	now := time.Now()
	for _, id := range order {
		t, ok := o.tasks[id]
		if !ok {
			continue
		}
		if t.Status == TaskStatusRunning {
			if cancel, hasCancel := o.cancelFuncs[id]; hasCancel && cancel != nil {
				cancel()
			}
			t.Status = TaskStatusAborted
			t.UpdatedAt = now

			if o.storage != nil {
				checkpointToSave := &storage.TaskCheckpoint{
					TaskID:         t.ID,
					SessionID:      t.SessionID,
					TaskType:       string(t.Type),
					Status:         string(TaskStatusAborted),
					CheckpointData: t.CheckpointData,
					UpdatedAt:      now,
				}
				_ = o.storage.Commit(ctx, func(tx storage.Transaction) error {
					return tx.SaveCheckpoint(checkpointToSave)
				})
			}
		}
	}

	return nil
}
