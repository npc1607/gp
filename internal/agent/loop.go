package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gp/internal/ai"
	"gp/internal/core/task"
	"gp/internal/tools"
)

// RunLoop 实现经典的 ReAct 状态循环：
// Prompt -> Model Stream -> ToolCall Request -> Execute Tool -> Append ToolResult -> Model Stream -> Finish
func RunLoop(ctx context.Context, a *AgentInstance, userInput string) (string, error) {
	if strings.TrimSpace(userInput) == "" {
		return "", fmt.Errorf("user prompt cannot be empty")
	}

	a.SetStatus(StatusPrompting)
	a.sessionTree.AppendMessage(ai.Message{
		Role:      ai.RoleUser,
		Content:   userInput,
		Timestamp: time.Now(),
	})

	sessTaskID := fmt.Sprintf("task_sess_%s", a.SessionID())
	taskTree := a.GetTaskTree()
	if taskTree != nil {
		sessCp, _ := json.Marshal(task.SessionCheckpoint{
			Prompt: userInput,
			Status: "running",
		})
		_ = taskTree.RegisterTask(ctx, &task.Task{
			ID:             sessTaskID,
			SessionID:      a.SessionID(),
			Type:           task.TaskTypeSession,
			Status:         task.TaskStatusRunning,
			CheckpointData: sessCp,
		}, nil)
	}

	return runReActLoop(ctx, a, sessTaskID)
}

// RunLoopResume 在断点恢复时继续推进现有会话的 ReAct 循环，不再重复添加用户提示词
func RunLoopResume(ctx context.Context, a *AgentInstance) (string, error) {
	sessTaskID := fmt.Sprintf("task_sess_%s", a.SessionID())
	taskTree := a.GetTaskTree()
	if taskTree != nil {
		if _, ok := taskTree.GetTask(sessTaskID); !ok {
			sessCp, _ := json.Marshal(task.SessionCheckpoint{
				Prompt: "(resumed)",
				Status: "running",
			})
			_ = taskTree.RegisterTask(ctx, &task.Task{
				ID:             sessTaskID,
				SessionID:      a.SessionID(),
				Type:           task.TaskTypeSession,
				Status:         task.TaskStatusRunning,
				CheckpointData: sessCp,
			}, nil)
		}
	}

	return runReActLoop(ctx, a, sessTaskID)
}

func runReActLoop(ctx context.Context, a *AgentInstance, sessTaskID string) (string, error) {
	taskTree := a.GetTaskTree()

	var toolDefs []json.RawMessage
	toolReg := a.toolReg
	if toolReg != nil {
		for _, t := range toolReg.List() {
			td := map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name(),
					"description": t.Description(),
					"parameters":  t.InputSchema(),
				},
			}
			bytes, err := json.Marshal(td)
			if err == nil {
				toolDefs = append(toolDefs, bytes)
			}
		}
	}

	maxTurns := a.GetMaxTurns()
	if maxTurns <= 0 {
		maxTurns = 50
	}

	for turn := 0; turn < maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			a.SetStatus(StatusSettled)
			if taskTree != nil {
				abortCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = taskTree.Abort(abortCtx, sessTaskID)
				cancel()
			}
			return "", err
		}

		genTaskID := fmt.Sprintf("task_gen_%s_%d", a.SessionID(), turn)
		if taskTree != nil {
			genCp, _ := json.Marshal(task.GenerationCheckpoint{
				Turn:  turn,
				Model: a.modelName,
			})
			_ = taskTree.RegisterTask(ctx, &task.Task{
				ID:             genTaskID,
				SessionID:      a.SessionID(),
				ParentID:       sessTaskID,
				Type:           task.TaskTypeGeneration,
				Status:         task.TaskStatusRunning,
				CheckpointData: genCp,
			}, nil)
		}

		messages := a.sessionTree.GetLinearContext()
		a.SetStatus(StatusStreaming)

		req := &ai.ChatRequest{
			Model:    a.modelName,
			Messages: messages,
			Tools:    toolDefs,
		}

		chunkChan, err := a.provider.StreamChat(ctx, req)
		if err != nil {
			a.SetStatus(StatusSettled)
			if taskTree != nil {
				_ = taskTree.UpdateTask(ctx, genTaskID, task.TaskStatusFailed, nil)
				_ = taskTree.UpdateTask(ctx, sessTaskID, task.TaskStatusFailed, nil)
			}
			return "", fmt.Errorf("model stream request failed: %w", err)
		}

		var textDelta strings.Builder
		var thinkingDelta strings.Builder

		type accToolCall struct {
			index    int
			id       string
			callType string
			name     string
			args     strings.Builder
		}

		toolCallsMap := make(map[int]*accToolCall)
		var toolOrder []int
		streamHandler := a.GetStreamHandler()
		var streamErr error

		for chunk := range chunkChan {
			if chunk.Error != nil {
				streamErr = chunk.Error
				continue
			}

			if chunk.DeltaText != "" {
				textDelta.WriteString(chunk.DeltaText)
			}
			if chunk.ThinkingDelta != "" {
				thinkingDelta.WriteString(chunk.ThinkingDelta)
			}

			if chunk.ToolCallDelta != nil {
				tc := chunk.ToolCallDelta
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				acc, exists := toolCallsMap[idx]
				if !exists {
					acc = &accToolCall{
						index:    idx,
						callType: tc.Type,
					}
					if acc.callType == "" {
						acc.callType = "function"
					}
					toolCallsMap[idx] = acc
					toolOrder = append(toolOrder, idx)
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Type != "" {
					acc.callType = tc.Type
				}
				if tc.Function.Name != "" {
					acc.name += tc.Function.Name
				}
				if len(tc.Function.Arguments) > 0 {
					acc.args.Write(tc.Function.Arguments)
				}
			}

			if streamHandler != nil {
				streamHandler(chunk)
			}
		}

		if streamErr != nil {
			a.SetStatus(StatusSettled)
			if taskTree != nil {
				_ = taskTree.UpdateTask(ctx, genTaskID, task.TaskStatusFailed, nil)
				_ = taskTree.UpdateTask(ctx, sessTaskID, task.TaskStatusFailed, nil)
			}
			return "", fmt.Errorf("stream chunk error: %w", streamErr)
		}

		if taskTree != nil {
			_ = taskTree.UpdateTask(ctx, genTaskID, task.TaskStatusCompleted, nil)
		}

		var finalToolCalls []ai.ToolCall
		for _, idx := range toolOrder {
			acc := toolCallsMap[idx]
			callID := acc.id
			if callID == "" {
				callID = fmt.Sprintf("call_%d_%d", turn, idx)
			}
			rawArgs := acc.args.String()
			if strings.TrimSpace(rawArgs) == "" {
				rawArgs = "{}"
			}
			finalToolCalls = append(finalToolCalls, ai.ToolCall{
				Index: &acc.index,
				ID:    callID,
				Type:  acc.callType,
				Function: ai.FunctionCall{
					Name:      acc.name,
					Arguments: json.RawMessage(rawArgs),
				},
			})
		}

		// 纯文本输出收敛退出
		if len(finalToolCalls) == 0 {
			a.sessionTree.AppendMessage(ai.Message{
				Role:      ai.RoleAssistant,
				Content:   textDelta.String(),
				Thinking:  thinkingDelta.String(),
				Timestamp: time.Now(),
			})
			a.SetStatus(StatusSettled)
			if taskTree != nil {
				_ = taskTree.UpdateTask(ctx, sessTaskID, task.TaskStatusCompleted, nil)
			}
			return textDelta.String(), nil
		}

		// 包含工具调用请求：先记录助手回复，再执行各工具调用
		a.sessionTree.AppendMessage(ai.Message{
			Role:      ai.RoleAssistant,
			Content:   textDelta.String(),
			Thinking:  thinkingDelta.String(),
			ToolCalls: finalToolCalls,
			Timestamp: time.Now(),
		})

		a.SetStatus(StatusToolExecuting)

		for _, call := range finalToolCalls {
			if err := ctx.Err(); err != nil {
				a.SetStatus(StatusSettled)
				if taskTree != nil {
					abortCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					_ = taskTree.Abort(abortCtx, sessTaskID)
					cancel()
				}
				return "", err
			}

			var replayMode tools.ReplayMode = tools.ReplayInterrupt
			var targetTool tools.Tool
			if a.toolReg != nil {
				if t, ok := a.toolReg.Get(call.Function.Name); ok {
					targetTool = t
					replayMode = tools.GetReplayMode(t)
				}
			}

			toolTaskID := fmt.Sprintf("task_tool_%s", call.ID)
			toolCpData, _ := json.Marshal(task.ToolCheckpoint{
				CallID:     call.ID,
				ToolName:   call.Function.Name,
				Arguments:  call.Function.Arguments,
				ReplayMode: replayMode,
				Turn:       turn,
			})

			// 【关键不变式】：执行工具前先原子落盘 Checkpoint (status = running)
			if taskTree != nil {
				_ = taskTree.RegisterTask(ctx, &task.Task{
					ID:             toolTaskID,
					SessionID:      a.SessionID(),
					ParentID:       genTaskID,
					Type:           task.TaskTypeTool,
					Status:         task.TaskStatusRunning,
					ReplayMode:     replayMode,
					CheckpointData: toolCpData,
				}, nil)
			}

			var resultContent string
			var hasExecErr bool
			if targetTool == nil {
				resultContent = fmt.Sprintf("Error: tool %q not found", call.Function.Name)
				hasExecErr = true
			} else {
				toolCtx := &tools.ToolContext{
					Context: ctx,
					AgentID: a.id,
					Cwd:     a.GetCwd(),
				}
				res, execErr := targetTool.Execute(toolCtx, call.Function.Arguments)
				if execErr != nil {
					resultContent = fmt.Sprintf("Tool %s execution error: %v", call.Function.Name, execErr)
					hasExecErr = true
				} else if res != nil {
					resultContent = res.Content
				}
			}

			// 【工具执行完成】：更新状态为 completed / failed
			if taskTree != nil {
				finalStatus := task.TaskStatusCompleted
				if hasExecErr {
					finalStatus = task.TaskStatusFailed
				}
				_ = taskTree.UpdateTask(ctx, toolTaskID, finalStatus, nil)
			}

			a.sessionTree.AppendMessage(ai.Message{
				Role:       ai.RoleTool,
				ToolCallID: call.ID,
				Content:    resultContent,
				Timestamp:  time.Now(),
			})
		}
	}

	a.SetStatus(StatusSettled)
	if taskTree != nil {
		_ = taskTree.UpdateTask(ctx, sessTaskID, task.TaskStatusFailed, nil)
	}
	return "", fmt.Errorf("agent exceeded maximum allowed turns (%d)", maxTurns)
}
