package task

import (
	"encoding/json"
	"time"

	"gp/internal/tools"
)

// TaskType 任务类型
type TaskType string

const (
	TaskTypeSession    TaskType = "session"
	TaskTypeGeneration TaskType = "generation"
	TaskTypeTool       TaskType = "tool"
	TaskTypeSubagent   TaskType = "subagent"
)

// TaskStatus 任务生命周期状态
type TaskStatus string

const (
	TaskStatusRunning     TaskStatus = "running"
	TaskStatusCompleted   TaskStatus = "completed"
	TaskStatusFailed      TaskStatus = "failed"
	TaskStatusAborted     TaskStatus = "aborted"
	TaskStatusInterrupted TaskStatus = "interrupted"
)

// Task 任务执行实例
type Task struct {
	ID             string           `json:"id"`
	SessionID      string           `json:"session_id"`
	ParentID       string           `json:"parent_id,omitempty"`
	Type           TaskType         `json:"type"`
	Status         TaskStatus       `json:"status"`
	ReplayMode     tools.ReplayMode `json:"replay_mode,omitempty"`
	CheckpointData json.RawMessage  `json:"checkpoint_data,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

// ToolCheckpoint 工具任务落盘检查点数据
type ToolCheckpoint struct {
	CallID     string           `json:"call_id"`
	ToolName   string           `json:"tool_name"`
	Arguments  json.RawMessage  `json:"arguments"`
	ReplayMode tools.ReplayMode `json:"replay_mode"`
	Turn       int              `json:"turn"`
}

// GenerationCheckpoint 大模型流式生成任务检查点数据
type GenerationCheckpoint struct {
	Turn  int    `json:"turn"`
	Model string `json:"model"`
}

// SessionCheckpoint 会话顶层任务检查点数据
type SessionCheckpoint struct {
	Prompt string `json:"prompt"`
	Status string `json:"status"`
}
