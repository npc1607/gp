package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"gp/internal/ai"
	"gp/internal/tools"
)

const (
	defaultObservationMaskWindow = 5
	minMaskBytes                 = 400
	minMaskLines                 = 8
	defaultRestoreMaxLines       = 200
)

// SetObservationMaskWindow configures how many recent tool-calling turns retain
// their full RoleTool outputs. Values <= 0 disable observation masking.
func (a *AgentInstance) SetObservationMaskWindow(turns int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.observationMaskWindow = turns
}

// GetObservationMaskWindow returns the active observation masking window size in turns.
func (a *AgentInstance) GetObservationMaskWindow() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.observationMaskWindow
}

// applyObservationMasking keeps tool outputs from the most recent windowTurns
// tool-calling assistant turns verbatim, and replaces older verbose RoleTool outputs
// with compact reversible DTOC placeholders without mutating SessionTree.
func applyObservationMasking(messages []ai.Message, windowTurns int) []ai.Message {
	if windowTurns <= 0 || len(messages) == 0 {
		return messages
	}

	// Walk backwards to locate the cutoff assistant message that starts the recent window.
	toolTurns := 0
	cutoffIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == ai.RoleAssistant && len(messages[i].ToolCalls) > 0 {
			toolTurns++
			if toolTurns == windowTurns {
				cutoffIdx = i
				break
			}
		}
	}
	if cutoffIdx <= 0 {
		return messages
	}

	// Map toolCallID -> toolName across the linear history for informative placeholders.
	callNames := make(map[string]string)
	for _, m := range messages {
		if m.Role == ai.RoleAssistant {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					callNames[tc.ID] = tc.Function.Name
				}
			}
		}
	}

	out := make([]ai.Message, len(messages))
	copy(out, messages)

	for i := 0; i < cutoffIdx; i++ {
		if out[i].Role != ai.RoleTool {
			continue
		}
		if !shouldMaskObservation(out[i].Content) {
			continue
		}
		toolName := callNames[out[i].ToolCallID]
		if toolName == "" {
			toolName = "unknown"
		}
		lines := countLines(out[i].Content)
		sizeStr := formatByteSize(len(out[i].Content))
		out[i].Content = fmt.Sprintf(
			"[已折叠历史工具输出: tool=%s call_id=%s | 原文 %d 行 / %s; 若后续确需查看原文，请调用 restore_observation(call_id=%q)]",
			toolName, out[i].ToolCallID, lines, sizeStr, out[i].ToolCallID,
		)
	}
	return out
}

func shouldMaskObservation(content string) bool {
	if len(content) <= minMaskBytes && countLines(content) <= minMaskLines {
		return false
	}
	return true
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func formatByteSize(b int) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	return fmt.Sprintf("%.1f KB", float64(b)/1024.0)
}

// restoreObservationTool implements DTOC on-demand restoration of masked historical
// tool outputs directly from the AgentInstance's SessionTree.
type restoreObservationTool struct {
	agent *AgentInstance
}

func (t *restoreObservationTool) Name() string { return "restore_observation" }

func (t *restoreObservationTool) Description() string {
	return "按 call_id 从会话历史中按需还原被折叠的早期工具输出原文（支持 start_line/end_line 分页，单次最多返回 200 行）。"
}

func (t *restoreObservationTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"call_id": {"type": "string", "description": "被折叠占位符中的 call_id"},
			"start_line": {"type": "integer", "description": "起始行号（1-based，默认 1）"},
			"end_line": {"type": "integer", "description": "结束行号（1-based，默认 start_line+199）"}
		},
		"required": ["call_id"]
	}`)
}

func (t *restoreObservationTool) ReplayMode() tools.ReplayMode {
	return tools.ReplaySafe
}

func (t *restoreObservationTool) Execute(_ *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	var args struct {
		CallID    string `json:"call_id"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &tools.ToolResult{Content: fmt.Sprintf("参数解析失败: %v", err), IsError: true}, nil
	}
	callID := strings.TrimSpace(args.CallID)
	if callID == "" {
		return &tools.ToolResult{Content: "缺少必填参数 call_id", IsError: true}, nil
	}

	t.agent.mu.RLock()
	st := t.agent.sessionTree
	t.agent.mu.RUnlock()
	if st == nil {
		return &tools.ToolResult{Content: "当前会话未启用 SessionTree，无法还原历史输出", IsError: true}, nil
	}

	var found string
	var ok bool
	msgs := st.GetLinearContext()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == ai.RoleTool && msgs[i].ToolCallID == callID {
			found = msgs[i].Content
			ok = true
			break
		}
	}
	if !ok {
		return &tools.ToolResult{
			Content: fmt.Sprintf("未在当前会话历史中找到 call_id=%q 的工具输出", callID),
			IsError: true,
		}, nil
	}

	allLines := strings.Split(found, "\n")
	total := len(allLines)
	start := args.StartLine
	if start < 1 {
		start = 1
	}
	if start > total {
		return &tools.ToolResult{
			Content: fmt.Sprintf("[call_id=%s | 共 %d 行，请求的 start_line=%d 超出范围]", callID, total, start),
		}, nil
	}
	end := args.EndLine
	if end < start {
		end = start + defaultRestoreMaxLines - 1
	}
	if end-start+1 > defaultRestoreMaxLines {
		end = start + defaultRestoreMaxLines - 1
	}
	if end > total {
		end = total
	}

	slice := strings.Join(allLines[start-1:end], "\n")
	header := fmt.Sprintf("[call_id=%s | 第 %d-%d 行，共 %d 行]", callID, start, end, total)
	if end < total {
		header += fmt.Sprintf("（后续还有 %d 行，可传 start_line=%d 继续查看）", total-end, end+1)
	}
	return &tools.ToolResult{Content: header + "\n" + slice}, nil
}
