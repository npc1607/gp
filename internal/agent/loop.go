package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gp/internal/ai"
	"gp/internal/core/compaction"
	"gp/internal/core/task"
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
	if strings.HasPrefix(a.id, "sub_") {
		// 子 Agent 生命周期已由父级 subagent ReplayInterrupt 任务兜底，跳过热路径 SQLite 写锁竞争
		taskTree = nil
	}

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
		maxTurns = 500
	}
	toolsTokens := estimateToolsTokens(toolDefs)
	a.setLastToolsTokens(toolsTokens)
	baseTurnIdx := a.nextMetricTurnBase()

	runStart := time.Now()
	runOutcome := "error"
	a.EmitEvent(AgentEvent{
		Type: EventTurnStart,
		Turn: 0,
	})
	defer func() {
		a.EmitEvent(AgentEvent{
			Type:     EventTurnEnd,
			Turn:     0,
			Duration: time.Since(runStart),
			Outcome:  runOutcome,
		})
	}()

	for turn := 0; turn < maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			runOutcome = "interrupted"
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

		assembled, ctxMeta := a.assembleModelMessagesWithMetrics()
		ctxMeta.toolsTokens = toolsTokens
		messages, compactedMeta, compacted := a.maybeAutoCompactWithMeta(ctx, assembled, toolsTokens)
		if compacted {
			ctxMeta = compactedMeta
		}
		a.SetStatus(StatusStreaming)

		req := &ai.ChatRequest{
			Model:         a.ModelName(),
			Messages:      messages,
			Tools:         toolDefs,
			ThinkingLevel: a.GetThinkingLevel(),
		}

		chunkChan, err := a.GetProvider().StreamChat(ctx, req)
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
		var thinkingSigDelta strings.Builder

		type accToolCall struct {
			index    int
			id       string
			callType string
			name     string
			args     strings.Builder
		}

		toolCallsMap := make(map[int]*accToolCall)
		idToIdx := make(map[string]int)
		var toolOrder []int
		nextSeqIdx := 0
		streamHandler := a.GetStreamHandler()
		var streamErr error
		var sawUsage bool
		var latestUsage *ai.TokenUsage

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
			if chunk.ThinkingSignature != "" {
				thinkingSigDelta.WriteString(chunk.ThinkingSignature)
			}

			if chunk.ToolCallDelta != nil {
				tc := chunk.ToolCallDelta
				var idx int
				switch {
				case tc.Index != nil:
					idx = *tc.Index
					if idx >= nextSeqIdx {
						nextSeqIdx = idx + 1
					}
					if tc.ID != "" {
						idToIdx[tc.ID] = idx
					}
				case tc.ID != "":
					if existing, ok := idToIdx[tc.ID]; ok {
						idx = existing
					} else {
						idx = nextSeqIdx
						nextSeqIdx++
						idToIdx[tc.ID] = idx
					}
				case tc.Function.Name != "" && len(toolOrder) > 0 && toolCallsMap[toolOrder[len(toolOrder)-1]].name != "" && toolCallsMap[toolOrder[len(toolOrder)-1]].name != tc.Function.Name:
					idx = nextSeqIdx
					nextSeqIdx++
				case len(toolOrder) > 0:
					idx = toolOrder[len(toolOrder)-1]
				default:
					idx = nextSeqIdx
					nextSeqIdx++
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
					if acc.name == "" {
						acc.name = tc.Function.Name
					} else if acc.name != tc.Function.Name && (tc.Index != nil || tc.ID != "") {
						acc.name += tc.Function.Name
					}
				}
				if len(tc.Function.Arguments) > 0 {
					acc.args.Write(tc.Function.Arguments)
				}
			}

			if chunk.Usage != nil {
				sawUsage = true
				uCopy := *chunk.Usage
				latestUsage = &uCopy
				a.EmitEvent(AgentEvent{
					Type:  EventUsage,
					Turn:  turn,
					Usage: chunk.Usage,
				})
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

		if !sawUsage {
			estIn := compaction.EstimateMessagesTokens(messages)
			estOut := compaction.EstimateTextTokens(textDelta.String()) + compaction.EstimateTextTokens(thinkingDelta.String())
			latestUsage = &ai.TokenUsage{
				InputTokens:  estIn,
				OutputTokens: estOut,
				TotalTokens:  estIn + estOut,
			}
			a.EmitEvent(AgentEvent{
				Type:  EventUsage,
				Turn:  turn,
				Usage: latestUsage,
			})
		}

		if taskTree != nil {
			_ = taskTree.UpdateTask(ctx, genTaskID, task.TaskStatusCompleted, nil)
		}

		var finalToolCalls []ai.ToolCall
		for _, idx := range toolOrder {
			acc := toolCallsMap[idx]
			callID := acc.id
			if callID == "" {
				callID = fmt.Sprintf("call_%d_%d_%d", time.Now().UnixNano(), turn, idx)
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
			a.recordTurnContextMetric(ctx, baseTurnIdx+turn+1, ctxMeta, latestUsage, 0, 0, 0)
			a.sessionTree.AppendMessage(ai.Message{
				Role:              ai.RoleAssistant,
				Content:           textDelta.String(),
				Thinking:          thinkingDelta.String(),
				ThinkingSignature: thinkingSigDelta.String(),
				Timestamp:         time.Now(),
			})
			a.SetStatus(StatusSettled)
			if taskTree != nil {
				_ = taskTree.UpdateTask(ctx, sessTaskID, task.TaskStatusCompleted, nil)
			}
			runOutcome = "completed"
			return textDelta.String(), nil
		}

		// 包含工具调用请求：先记录助手回复，再执行各工具调用
		a.sessionTree.AppendMessage(ai.Message{
			Role:              ai.RoleAssistant,
			Content:           textDelta.String(),
			Thinking:          thinkingDelta.String(),
			ThinkingSignature: thinkingSigDelta.String(),
			ToolCalls:         finalToolCalls,
			Timestamp:         time.Now(),
		})

		a.SetStatus(StatusToolExecuting)

		recallCalls, restoreCalls, searchCalls := 0, 0, 0
		for _, tc := range finalToolCalls {
			switch tc.Function.Name {
			case "recall_entries":
				recallCalls++
			case "restore_observation":
				restoreCalls++
			case "search_context":
				searchCalls++
			}
		}

		outcomes, execBatchErr := executeToolBatch(ctx, a, turn, genTaskID, sessTaskID, taskTree, finalToolCalls)
		if execBatchErr != nil {
			a.SetStatus(StatusSettled)
			return "", execBatchErr
		}
		for _, out := range outcomes {
			a.sessionTree.AppendMessage(ai.Message{
				Role:       ai.RoleTool,
				ToolCallID: out.callID,
				Content:    out.content,
				Images:     out.images,
				Timestamp:  time.Now(),
			})
		}
		a.recordTurnContextMetric(ctx, baseTurnIdx+turn+1, ctxMeta, latestUsage, recallCalls, restoreCalls, searchCalls)
	}

	a.SetStatus(StatusSettled)
	if taskTree != nil {
		_ = taskTree.UpdateTask(ctx, sessTaskID, task.TaskStatusFailed, nil)
	}
	return "", fmt.Errorf("agent exceeded maximum allowed turns (%d)", maxTurns)
}

func estimateToolsTokens(toolDefs []json.RawMessage) int64 {
	var total int64
	for _, raw := range toolDefs {
		total += compaction.EstimateTextTokens(string(raw)) + 8
	}
	return total
}
