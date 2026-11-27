package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gp/internal/ai"
	"gp/internal/core/task"
	"gp/internal/tools"
)

// maxToolResultBytes 单个工具返回写入会话历史的最大字节数（64KB），防止大输出撑爆上下文窗口
const maxToolResultBytes = 64 * 1024

type toolExecOutcome struct {
	callID     string
	content    string
	images     []ai.ImagePart
	isError    bool
	replayMode tools.ReplayMode
}

// truncateToolOutput 对超大工具返回执行 UTF-8 边界安全的头尾保留截断
func truncateToolOutput(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	headBudget := maxBytes * 3 / 4
	tailBudget := maxBytes - headBudget

	headEnd := headBudget
	for headEnd > 0 && !utf8.RuneStart(s[headEnd]) {
		headEnd--
	}
	tailStart := len(s) - tailBudget
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	if tailStart <= headEnd {
		return s[:headEnd]
	}
	omitted := tailStart - headEnd
	return fmt.Sprintf("%s\n\n...[tool output truncated: %d bytes omitted (total %d bytes)]...\n\n%s",
		s[:headEnd], omitted, len(s), s[tailStart:])
}

// executeToolBatch 执行单轮全部工具调用：连续 ReplaySafe 工具并发执行，ReplayInterrupt 工具按序栅栏执行。
func executeToolBatch(
	ctx context.Context,
	a *AgentInstance,
	turn int,
	genTaskID, sessTaskID string,
	taskTree *task.OwnershipTree,
	calls []ai.ToolCall,
) ([]toolExecOutcome, error) {
	outcomes := make([]toolExecOutcome, len(calls))
	isChildAgent := strings.HasPrefix(a.id, "sub_")

	i := 0
	for i < len(calls) {
		if err := ctx.Err(); err != nil {
			abortTaskTree(taskTree, sessTaskID)
			return nil, err
		}

		// 查找当前位置起的连续 ReplaySafe 区间
		if resolveCallReplayMode(a, calls[i].Function.Name) == tools.ReplaySafe {
			j := i
			for j < len(calls) && resolveCallReplayMode(a, calls[j].Function.Name) == tools.ReplaySafe {
				j++
			}
			if j-i == 1 {
				outcomes[i] = executeSingleTool(ctx, a, turn, genTaskID, taskTree, isChildAgent, calls[i])
			} else {
				var wg sync.WaitGroup
				for k := i; k < j; k++ {
					wg.Add(1)
					go func(idx int) {
						defer wg.Done()
						outcomes[idx] = executeSingleTool(ctx, a, turn, genTaskID, taskTree, isChildAgent, calls[idx])
					}(k)
				}
				wg.Wait()
			}
			i = j
			continue
		}

		// ReplayInterrupt 工具按顺序串行执行，保障副作用与 Checkpoint 原子性
		outcomes[i] = executeSingleTool(ctx, a, turn, genTaskID, taskTree, isChildAgent, calls[i])
		i++
	}

	if err := ctx.Err(); err != nil {
		abortTaskTree(taskTree, sessTaskID)
		return nil, err
	}
	return outcomes, nil
}

func resolveCallReplayMode(a *AgentInstance, toolName string) tools.ReplayMode {
	if a.toolReg != nil {
		if t, ok := a.toolReg.Get(toolName); ok {
			return tools.GetReplayMode(t)
		}
	}
	return tools.ReplayInterrupt
}

func executeSingleTool(
	ctx context.Context,
	a *AgentInstance,
	turn int,
	genTaskID string,
	taskTree *task.OwnershipTree,
	isChildAgent bool,
	call ai.ToolCall,
) toolExecOutcome {
	replayMode := tools.ReplayInterrupt
	var targetTool tools.Tool
	if a.toolReg != nil {
		if t, ok := a.toolReg.Get(call.Function.Name); ok {
			targetTool = t
			replayMode = tools.GetReplayMode(t)
		}
	}

	// 热路径优化：仅主 Agent 的 ReplayInterrupt 副作用工具需要同步写入 SQLite Checkpoint；
	// ReplaySafe 只读工具与子 Agent 跳过同步写锁，避免并发退化为串行。
	shouldCheckpoint := taskTree != nil && !isChildAgent && replayMode == tools.ReplayInterrupt
	toolTaskID := fmt.Sprintf("task_tool_%s", call.ID)
	if shouldCheckpoint {
		toolCpData, _ := json.Marshal(task.ToolCheckpoint{
			CallID:     call.ID,
			ToolName:   call.Function.Name,
			Arguments:  call.Function.Arguments,
			ReplayMode: replayMode,
			Turn:       turn,
		})
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
	var resultImages []ai.ImagePart
	var hasExecErr bool
	toolStart := time.Now()
	a.EmitEvent(AgentEvent{
		Type:     EventToolCallStart,
		Turn:     turn,
		CallID:   call.ID,
		ToolName: call.Function.Name,
		Args:     call.Function.Arguments,
	})
	if targetTool == nil {
		resultContent = fmt.Sprintf("Error: tool %q not found", call.Function.Name)
		hasExecErr = true
	} else {
		evCh := make(chan tools.ToolEvent, 8)
		evDone := make(chan struct{})
		go func() {
			defer close(evDone)
			for ev := range evCh {
				a.EmitEvent(AgentEvent{
					Type:     EventToolProgress,
					Turn:     turn,
					CallID:   call.ID,
					ToolName: ev.ToolName,
					Result:   ev.Action,
					IsError:  ev.IsError,
					Stalled:  ev.Stalled,
				})
			}
		}()

		toolCtx := &tools.ToolContext{
			Context:    ctx,
			AgentID:    a.id,
			SessionID:  a.SessionID(),
			IsSubagent: a.IsSubagent(),
			Cwd:        a.GetCwd(),
			EventChan:  evCh,
		}
		res, execErr := targetTool.Execute(toolCtx, call.Function.Arguments)
		close(evCh)
		<-evDone
		if execErr != nil {
			resultContent = fmt.Sprintf("Tool %s execution error: %v", call.Function.Name, execErr)
			hasExecErr = true
		} else if res != nil {
			resultContent = truncateToolOutput(res.Content, maxToolResultBytes)
			if len(res.Images) > 0 {
				resultImages = make([]ai.ImagePart, 0, len(res.Images))
				for _, img := range res.Images {
					resultImages = append(resultImages, ai.ImagePart{
						MimeType:   img.MimeType,
						Base64Data: img.Base64Data,
						FileName:   img.FileName,
					})
				}
			}
		}
	}
	a.EmitEvent(AgentEvent{
		Type:     EventToolCallEnd,
		Turn:     turn,
		CallID:   call.ID,
		ToolName: call.Function.Name,
		Args:     call.Function.Arguments,
		Result:   resultContent,
		IsError:  hasExecErr,
		Duration: time.Since(toolStart),
	})

	if shouldCheckpoint {
		finalStatus := task.TaskStatusCompleted
		if hasExecErr {
			finalStatus = task.TaskStatusFailed
		}
		_ = taskTree.UpdateTask(ctx, toolTaskID, finalStatus, nil)
	}

	return toolExecOutcome{
		callID:     call.ID,
		content:    resultContent,
		images:     resultImages,
		isError:    hasExecErr,
		replayMode: replayMode,
	}
}

func abortTaskTree(taskTree *task.OwnershipTree, sessTaskID string) {
	if taskTree == nil {
		return
	}
	abortCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = taskTree.Abort(abortCtx, sessTaskID)
	cancel()
}
