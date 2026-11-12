package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"gp/internal/ai"
	"gp/internal/core/session"
	"gp/internal/core/storage"
	"gp/internal/core/task"
	"gp/internal/tools"
)

// ErrNoUnfinishedTask 没有发现未完成的任务
var ErrNoUnfinishedTask = errors.New("no unfinished tasks found to resume")

// ResumeOptions 恢复执行选项
type ResumeOptions struct {
	Storage       storage.Storage
	Provider      ai.Provider
	Registry      *tools.Registry
	SessionID     string // 可选，若为空则自动选取最近未完成的任务所在的 session
	Writer        io.Writer
	StreamHandler func(chunk ai.StreamChunk)
	Cwd           string
	ModelName     string
}

// ResumeResult 恢复执行结果
type ResumeResult struct {
	SessionID     string
	FinalResponse string
	RecoveredTask string
	ReplayAction  string // "safe_rerun", "interrupted_reported", "completed"
}

// Resume 扫描未完成任务，依照幂等策略恢复执行并推进 ReAct 循环至收敛
func Resume(ctx context.Context, opts ResumeOptions) (*ResumeResult, error) {
	if opts.Storage == nil {
		return nil, fmt.Errorf("storage is required for resume")
	}
	if opts.Provider == nil {
		return nil, fmt.Errorf("provider is required for resume")
	}

	unfinished, err := opts.Storage.ListUnfinishedCheckpoints()
	if err != nil {
		return nil, fmt.Errorf("failed to list unfinished checkpoints: %w", err)
	}

	if len(unfinished) == 0 {
		return nil, ErrNoUnfinishedTask
	}

	targetSessionID := opts.SessionID
	if targetSessionID == "" {
		// 默认选取最新一条未完成任务的 session
		targetSessionID = unfinished[len(unfinished)-1].SessionID
	}

	// 过滤属于该 session 的未完成任务
	var sessionUnfinished []*storage.TaskCheckpoint
	for _, cp := range unfinished {
		if cp.SessionID == targetSessionID {
			sessionUnfinished = append(sessionUnfinished, cp)
		}
	}
	if len(sessionUnfinished) == 0 {
		return nil, ErrNoUnfinishedTask
	}

	cwd := opts.Cwd
	if cwd == "" {
		if dir, err := os.Getwd(); err == nil {
			cwd = dir
		} else {
			cwd = "."
		}
	}

	st, err := session.NewSessionTree(targetSessionID, opts.Storage)
	if err != nil {
		return nil, fmt.Errorf("failed to restore session tree: %w", err)
	}

	tree := task.NewOwnershipTree(opts.Storage)
	ag := NewAgent(targetSessionID, "assistant", opts.Provider, opts.ModelName, opts.Registry)
	ag.SetSessionTree(st)
	ag.SetTaskTree(tree)
	ag.SetStorage(opts.Storage)
	ag.SetCwd(cwd)
	if opts.StreamHandler != nil {
		ag.SetStreamHandler(opts.StreamHandler)
	}

	replayAction := "none"
	var lastRecoveredTaskID string

	for _, cp := range sessionUnfinished {
		lastRecoveredTaskID = cp.TaskID
		t, convErr := task.ConvertStorageCheckpointToTask(cp)
		if convErr != nil {
			continue
		}

		switch t.Type {
		case task.TaskTypeTool:
			toolCp, decodeErr := task.DecodeToolCheckpoint(t.CheckpointData)
			if decodeErr != nil {
				// 损坏的工具检查点，标记中断
				_ = tree.UpdateTask(ctx, t.ID, task.TaskStatusInterrupted, nil)
				continue
			}

			if toolCp.ReplayMode == tools.ReplaySafe {
				// 【安全只读工具：自动重跑】
				replayAction = string(task.ActionSafeRerun)
				var resultContent string
				var hasExecErr bool

				if opts.Registry != nil {
					if targetTool, ok := opts.Registry.Get(toolCp.ToolName); ok {
						toolCtx := &tools.ToolContext{
							Context: ctx,
							AgentID: targetSessionID,
							Cwd:     cwd,
						}
						res, execErr := targetTool.Execute(toolCtx, toolCp.Arguments)
						if execErr != nil {
							resultContent = fmt.Sprintf("Tool %s execution error: %v", toolCp.ToolName, execErr)
							hasExecErr = true
						} else if res != nil {
							resultContent = res.Content
						}
					} else {
						resultContent = fmt.Sprintf("Error: tool %q not found in registry", toolCp.ToolName)
						hasExecErr = true
					}
				} else {
					resultContent = "Error: no tool registry available for resume"
					hasExecErr = true
				}

				finalStatus := task.TaskStatusCompleted
				if hasExecErr {
					finalStatus = task.TaskStatusFailed
				}
				_ = tree.UpdateTask(ctx, t.ID, finalStatus, nil)

				st.AppendMessage(ai.Message{
					Role:       ai.RoleTool,
					ToolCallID: toolCp.CallID,
					Content:    resultContent,
				})

			} else {
				// 【有副作用工具：不重复执行，注入安全中断提示由大模型决策】
				replayAction = string(task.ActionInterruptReported)
				_ = tree.UpdateTask(ctx, t.ID, task.TaskStatusInterrupted, nil)

				notice := fmt.Sprintf("Tool call %q (%s) was interrupted by system shutdown/crash before completion. To avoid unintended side-effects, the tool was not re-executed. Please inspect workspace state and decide the next action.", toolCp.CallID, toolCp.ToolName)
				st.AppendMessage(ai.Message{
					Role:       ai.RoleTool,
					ToolCallID: toolCp.CallID,
					Content:    notice,
				})
			}

		case task.TaskTypeGeneration:
			// 未竟的大模型流式输出，标记为中断
			_ = tree.UpdateTask(ctx, t.ID, task.TaskStatusInterrupted, nil)

		case task.TaskTypeSession:
			// 根会话任务保持或重新置为 running
		}
	}

	// 驱动模型从恢复点继续推理
	finalResp, err := RunLoopResume(ctx, ag)
	if err != nil {
		return nil, fmt.Errorf("resume ReAct loop failed: %w", err)
	}

	return &ResumeResult{
		SessionID:     targetSessionID,
		FinalResponse: finalResp,
		RecoveredTask: lastRecoveredTaskID,
		ReplayAction:  replayAction,
	}, nil
}
