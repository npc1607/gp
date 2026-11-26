package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"gp/internal/ai"
	"gp/internal/tools"
)

const (
	DefaultObservationMaskWindow = 5
	DefaultObservationMaskEpoch  = 16

	defaultObservationMaskWindow = DefaultObservationMaskWindow
	defaultObservationMaskEpoch  = DefaultObservationMaskEpoch
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

// SetObservationMaskEpoch configures the discrete Epoch stride for frozen observation masking.
// Default is 16; values <= 0 disable epoch alignment (falling back to window-only masking).
func (a *AgentInstance) SetObservationMaskEpoch(epoch int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.observationMaskEpoch = epoch
}

// GetObservationMaskEpoch returns the active Epoch stride for frozen observation masking.
func (a *AgentInstance) GetObservationMaskEpoch() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.observationMaskEpoch
}

// applyObservationMasking keeps tool outputs from the most recent windowTurns
// tool-calling assistant turns verbatim, and replaces older verbose RoleTool outputs
// with compact reversible DTOC placeholders without mutating SessionTree.
func applyObservationMasking(messages []ai.Message, windowTurns int) []ai.Message {
	if windowTurns <= 0 || len(messages) == 0 {
		return messages
	}

	cutoffIdx := findToolWindowCutoff(messages, windowTurns)
	if cutoffIdx <= 0 {
		return messages
	}

	out, _ := maskToolOutputsBefore(messages, cutoffIdx)
	return out
}

// ApplyEpochObservationMaskingWithStats applies Epoch-frozen masking and returns the masked slice,
// active epochID, freezeBoundary index, and number of folded tool outputs.
func ApplyEpochObservationMaskingWithStats(messages []ai.Message, epochStride, minRecentToolTurns int) (out []ai.Message, epochID int, freezeBoundary int, maskedCount int) {
	out, epochID, freezeBoundary, maskedCount, _ = ApplyEpochObservationMaskingWithOffset(messages, epochStride, minRecentToolTurns, 0)
	return out, epochID, freezeBoundary, maskedCount
}

// ApplyEpochObservationMaskingWithOffset applies Epoch-frozen masking using priorToolTurnOffset
// (count of compacted historical tool-calling turns prior to messages[0]) so Epoch progress remains
// monotonic across compactions and advances strictly by logical tool-calling turns (epochStride tool
// turns per Epoch).
func ApplyEpochObservationMaskingWithOffset(messages []ai.Message, epochStride, minRecentToolTurns, priorToolTurnOffset int) (out []ai.Message, epochID int, freezeBoundary int, maskedCount int, epochPos int) {
	if priorToolTurnOffset < 0 {
		priorToolTurnOffset = 0
	}
	if minRecentToolTurns <= 0 || len(messages) == 0 {
		if epochStride > 1 && priorToolTurnOffset > 0 {
			epochID = priorToolTurnOffset / epochStride
			epochPos = priorToolTurnOffset % epochStride
		}
		return messages, epochID, 0, 0, epochPos
	}
	if epochStride <= 1 {
		cutoff := FindToolWindowCutoff(messages, minRecentToolTurns)
		if cutoff <= 0 {
			return messages, 0, 0, 0, 0
		}
		out, maskedCount = maskToolOutputsBefore(messages, cutoff)
		return out, 0, cutoff, maskedCount, 0
	}

	toolIndices := findToolTurnIndices(messages)
	localToolTurns := len(toolIndices)
	globalTotalTurns := priorToolTurnOffset + localToolTurns

	globalCutoffTurn := globalTotalTurns - minRecentToolTurns
	if globalCutoffTurn < 0 {
		globalCutoffTurn = 0
	}

	if priorToolTurnOffset > 0 || localToolTurns > minRecentToolTurns {
		globalFreezeTurn := (globalCutoffTurn / epochStride) * epochStride
		epochID = globalFreezeTurn / epochStride
		epochPos = globalCutoffTurn - globalFreezeTurn

		localFreezeTurn := globalFreezeTurn - priorToolTurnOffset
		if localFreezeTurn <= 0 {
			return messages, epochID, 0, 0, epochPos
		}
		if localFreezeTurn >= localToolTurns {
			freezeBoundary = len(messages)
		} else {
			freezeBoundary = toolIndices[localFreezeTurn]
		}
		if freezeBoundary <= 0 {
			return messages, epochID, 0, 0, epochPos
		}
		out, maskedCount = maskToolOutputsBefore(messages, freezeBoundary)
		return out, epochID, freezeBoundary, maskedCount, epochPos
	}

	// Fallback safeguard: when a conversation has at most minRecentToolTurns tool calls followed
	// by a long non-tool tail while still holding a maskable oversized tool output, fold prior to
	// the recent message tail once conversation length reaches epochStride + 2*minRecentToolTurns.
	if hasMaskableToolOutput(messages) && len(messages) >= epochStride+minRecentToolTurns*2 {
		if fallback := findTailMessageCutoff(messages, minRecentToolTurns*2); fallback > 0 {
			out, maskedCount = maskToolOutputsBefore(messages, fallback)
			return out, epochID, fallback, maskedCount, epochPos
		}
	}

	return messages, epochID, 0, 0, epochPos
}

func findToolTurnIndices(messages []ai.Message) []int {
	var indices []int
	for i, m := range messages {
		if m.Role == ai.RoleAssistant && len(m.ToolCalls) > 0 {
			indices = append(indices, i)
		}
	}
	return indices
}

func hasMaskableToolOutput(messages []ai.Message) bool {
	for _, m := range messages {
		if m.Role == ai.RoleTool && shouldMaskObservation(m.Content) {
			return true
		}
	}
	return false
}

func findTailMessageCutoff(messages []ai.Message, keepTail int) int {
	if len(messages) <= keepTail || keepTail <= 0 {
		return -1
	}
	idx := len(messages) - keepTail
	// Avoid splitting right after an assistant tool_call before its tool_result.
	for idx > 0 && messages[idx].Role == ai.RoleTool {
		idx--
	}
	return idx
}

// FindToolWindowCutoff locates the start index of the most recent windowTurns tool-calling turns.
func FindToolWindowCutoff(messages []ai.Message, windowTurns int) int {
	return findToolWindowCutoff(messages, windowTurns)
}

func findToolWindowCutoff(messages []ai.Message, windowTurns int) int {
	toolTurns := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == ai.RoleAssistant && len(messages[i].ToolCalls) > 0 {
			toolTurns++
			if toolTurns == windowTurns {
				return i
			}
		}
	}
	return -1
}

func maskToolOutputsBefore(messages []ai.Message, boundary int) ([]ai.Message, int) {
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

	maskedCount := 0
	for i := 0; i < boundary; i++ {
		if out[i].Role != ai.RoleTool {
			continue
		}
		if strings.HasPrefix(out[i].Content, "[已折叠历史工具输出:") || strings.HasPrefix(out[i].Content, "[compacted tool output:") {
			maskedCount++
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
		maskedCount++
	}
	return out, maskedCount
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
	entries := st.GetBranchEntries()
	for i := len(entries) - 1; i >= 0; i-- {
		m := entries[i].Message
		if m.Role == ai.RoleTool && m.ToolCallID == callID {
			found = m.Content
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
