package task

import (
	"encoding/json"
	"fmt"

	"gp/internal/core/storage"
	"gp/internal/tools"
)

// DecodeToolCheckpoint 反序列化工具检查点
func DecodeToolCheckpoint(raw json.RawMessage) (*ToolCheckpoint, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty checkpoint data")
	}
	var cp ToolCheckpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, fmt.Errorf("failed to decode tool checkpoint: %w", err)
	}
	return &cp, nil
}

// ConvertStorageCheckpointToTask 将持久层 Checkpoint 转换为核心 Task
func ConvertStorageCheckpointToTask(cp *storage.TaskCheckpoint) (*Task, error) {
	if cp == nil {
		return nil, fmt.Errorf("nil checkpoint")
	}
	t := &Task{
		ID:             cp.TaskID,
		SessionID:      cp.SessionID,
		Type:           TaskType(cp.TaskType),
		Status:         TaskStatus(cp.Status),
		CheckpointData: cp.CheckpointData,
		UpdatedAt:      cp.UpdatedAt,
	}

	if t.Type == TaskTypeTool && len(cp.CheckpointData) > 0 {
		if toolCp, err := DecodeToolCheckpoint(cp.CheckpointData); err == nil {
			t.ReplayMode = toolCp.ReplayMode
		}
	}
	return t, nil
}

// CheckReplayAction 根据任务类型与检查点数据评估恢复时的重放动作
type ReplayAction string

const (
	ActionSafeRerun         ReplayAction = "safe_rerun"
	ActionInterruptReported ReplayAction = "interrupt_reported"
	ActionSkip              ReplayAction = "skip"
)

func ResolveReplayAction(t *Task) (ReplayAction, *ToolCheckpoint, error) {
	if t == nil {
		return ActionSkip, nil, fmt.Errorf("task is nil")
	}
	if t.Type != TaskTypeTool {
		return ActionInterruptReported, nil, nil
	}

	toolCp, err := DecodeToolCheckpoint(t.CheckpointData)
	if err != nil {
		return ActionInterruptReported, nil, err
	}

	if toolCp.ReplayMode == tools.ReplaySafe {
		return ActionSafeRerun, toolCp, nil
	}
	return ActionInterruptReported, toolCp, nil
}
