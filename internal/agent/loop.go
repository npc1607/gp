package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gp/internal/ai"
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

	var toolDefs []json.RawMessage
	if a.toolReg != nil {
		for _, t := range a.toolReg.List() {
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
			return "", err
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
			return "", fmt.Errorf("stream chunk error: %w", streamErr)
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
			var resultContent string
			if a.toolReg == nil {
				resultContent = "Error: no tool registry available"
			} else if tool, ok := a.toolReg.Get(call.Function.Name); !ok {
				resultContent = fmt.Sprintf("Error: tool %q not found", call.Function.Name)
			} else {
				toolCtx := &tools.ToolContext{
					Context: ctx,
					AgentID: a.id,
					Cwd:     a.GetCwd(),
				}
				res, execErr := tool.Execute(toolCtx, call.Function.Arguments)
				if execErr != nil {
					resultContent = fmt.Sprintf("Tool %s execution error: %v", call.Function.Name, execErr)
				} else if res != nil {
					resultContent = res.Content
				}
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
	return "", fmt.Errorf("agent exceeded maximum allowed turns (%d)", maxTurns)
}
