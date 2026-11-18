package compaction

import (
	"context"
	"fmt"
	"strings"

	"gp/internal/ai"
)

// Options 配置两级上下文压缩策略
type Options struct {
	KeepRecentTurns    int    // Stage 1 保留完整工具输出的最近用户轮次数（默认 4）
	KeepRecentMessages int    // Stage 2 保留不压缩的尾部消息最小条数（默认 6）
	Focus              string // 可选用户指定的摘要保留重点
}

// Result 描述一次上下文压缩的执行结果
type Result struct {
	Summary         string
	KeptMessages    []ai.Message
	BeforeTokens    int64
	AfterTokens     int64
	PrunedToolCount int
	UsedLLM         bool
}

// PruneToolOutputs Stage 1：无损/微损裁剪早期大段工具输出（不调用 LLM）。
// 保留最近 keepRecentTurns 轮用户交互内的完整工具输出，更早且超过 400 字节的 tool 结果折叠为轻量占位。
func PruneToolOutputs(msgs []ai.Message, keepRecentTurns int) ([]ai.Message, int) {
	if keepRecentTurns <= 0 {
		keepRecentTurns = 4
	}
	cutoff := findRecentTurnStart(msgs, keepRecentTurns)
	out := make([]ai.Message, len(msgs))
	copy(out, msgs)

	pruned := 0
	for i := range cutoff {
		if out[i].Role != ai.RoleTool {
			continue
		}
		if len(out[i].Content) <= 400 {
			continue
		}
		lines := strings.Count(out[i].Content, "\n") + 1
		out[i].Content = fmt.Sprintf("[compacted tool output: %d lines, %d bytes, call_id=%s]",
			lines, len(out[i].Content), out[i].ToolCallID)
		pruned++
	}
	return out, pruned
}

// Compact 执行两级混合上下文压缩：先做 Stage 1 工具输出裁剪，再对早期历史生成结构化摘要。
func Compact(ctx context.Context, prov ai.Provider, modelName string, msgs []ai.Message, opts Options) (*Result, error) {
	if opts.KeepRecentTurns <= 0 {
		opts.KeepRecentTurns = 4
	}
	if opts.KeepRecentMessages <= 0 {
		opts.KeepRecentMessages = 6
	}

	beforeTokens := EstimateMessagesTokens(msgs)
	prunedMsgs, prunedCount := PruneToolOutputs(msgs, opts.KeepRecentTurns)

	// 按 User 消息边界切分：保留尾部最近对话，不打断 tool_call / tool_result 配对
	splitIdx := findTailSplitIndex(prunedMsgs, opts.KeepRecentMessages)
	if splitIdx <= 0 {
		// 消息过少，仅返回 Stage 1 裁剪结果
		after := EstimateMessagesTokens(prunedMsgs)
		return &Result{
			Summary:         "",
			KeptMessages:    prunedMsgs,
			BeforeTokens:    beforeTokens,
			AfterTokens:     after,
			PrunedToolCount: prunedCount,
		}, nil
	}

	head := prunedMsgs[:splitIdx]
	tail := prunedMsgs[splitIdx:]

	summary, usedLLM := summarizeHead(ctx, prov, modelName, head, opts.Focus)
	summaryMsg := BuildSummaryMessage(summary)
	afterTokens := EstimateMessageTokens(summaryMsg) + EstimateMessagesTokens(tail)

	return &Result{
		Summary:         summary,
		KeptMessages:    tail,
		BeforeTokens:    beforeTokens,
		AfterTokens:     afterTokens,
		PrunedToolCount: prunedCount,
		UsedLLM:         usedLLM,
	}, nil
}

// BuildSummaryMessage 将压缩摘要封装为注入给大模型的用户侧上下文消息
func BuildSummaryMessage(summary string) ai.Message {
	return ai.Message{
		Role:    ai.RoleUser,
		Content: "<compacted_context>\n" + strings.TrimSpace(summary) + "\n</compacted_context>",
	}
}

func findRecentTurnStart(msgs []ai.Message, keepTurns int) int {
	if keepTurns <= 0 || len(msgs) == 0 {
		return len(msgs)
	}
	seen := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == ai.RoleUser {
			seen++
			if seen >= keepTurns {
				return i
			}
		}
	}
	return 0
}

func findTailSplitIndex(msgs []ai.Message, minKeep int) int {
	if len(msgs) <= minKeep {
		return 0
	}
	target := len(msgs) - minKeep
	// 向前/向后对齐到最近的 RoleUser 边界，避免把工具调用链截断
	for i := target; i >= 1; i-- {
		if msgs[i].Role == ai.RoleUser {
			return i
		}
	}
	for i := target + 1; i < len(msgs); i++ {
		if msgs[i].Role == ai.RoleUser {
			return i
		}
	}
	return 0
}

func summarizeHead(ctx context.Context, prov ai.Provider, modelName string, head []ai.Message, focus string) (string, bool) {
	if prov != nil && modelName != "" {
		if sum, err := callLLMSummary(ctx, prov, modelName, head, focus); err == nil && strings.TrimSpace(sum) != "" {
			return strings.TrimSpace(sum), true
		}
	}
	return buildExtractiveFallbackSummary(head, focus), false
}

func callLLMSummary(ctx context.Context, prov ai.Provider, modelName string, head []ai.Message, focus string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var transcript strings.Builder
	for _, m := range head {
		if m.Role == ai.RoleSystem {
			continue
		}
		content := strings.TrimSpace(m.Content)
		if content == "" && len(m.ToolCalls) == 0 {
			continue
		}
		if len(content) > 1500 {
			content = content[:1500] + "\n…[truncated]"
		}
		fmt.Fprintf(&transcript, "[%s]: %s\n", m.Role, content)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&transcript, "  -> tool_call %s(%s)\n", tc.Function.Name, string(tc.Function.Arguments))
		}
	}

	focusBlock := ""
	if strings.TrimSpace(focus) != "" {
		focusBlock = "\nAdditional focus requested by user: " + strings.TrimSpace(focus) + "\n"
	}

	prompt := fmt.Sprintf(`Summarize the following conversation history into a concise, high-signal structured handoff so the coding agent can continue seamlessly without losing technical details.%s
Use exactly these 5 sections:
1. Goal & Constraints
2. Key Files & Symbols
3. Completed Changes & Decisions
4. Open Issues / Errors
5. Next Steps

Conversation history:
%s`, focusBlock, transcript.String())

	ch, err := prov.StreamChat(ctx, &ai.ChatRequest{
		Model: modelName,
		Messages: []ai.Message{
			{Role: ai.RoleSystem, Content: "You are a precise technical context compactor. Preserve exact file paths, symbol names, commands, and error messages."},
			{Role: ai.RoleUser, Content: prompt},
		},
	})
	if err != nil {
		return "", err
	}

	var out strings.Builder
	for chunk := range ch {
		if chunk.Error != nil {
			return "", chunk.Error
		}
		out.WriteString(chunk.DeltaText)
	}
	return out.String(), nil
}

func buildExtractiveFallbackSummary(head []ai.Message, focus string) string {
	var sb strings.Builder
	sb.WriteString("1. Goal & Constraints\n")
	if strings.TrimSpace(focus) != "" {
		fmt.Fprintf(&sb, "- Focus: %s\n", strings.TrimSpace(focus))
	}
	userCount := 0
	for _, m := range head {
		if m.Role == ai.RoleUser && strings.TrimSpace(m.Content) != "" {
			line := strings.ReplaceAll(strings.TrimSpace(m.Content), "\n", " ")
			if len(line) > 160 {
				line = line[:160] + "…"
			}
			fmt.Fprintf(&sb, "- User: %s\n", line)
			userCount++
			if userCount >= 5 {
				break
			}
		}
	}
	sb.WriteString("\n2. Key Files & Symbols\n- (Preserved from compacted history)\n")
	sb.WriteString("\n3. Completed Changes & Decisions\n")
	toolCalls := 0
	for _, m := range head {
		for _, tc := range m.ToolCalls {
			toolCalls++
			if toolCalls <= 8 {
				fmt.Fprintf(&sb, "- Tool: %s\n", tc.Function.Name)
			}
		}
	}
	if toolCalls == 0 {
		sb.WriteString("- Earlier dialogue compacted\n")
	}
	sb.WriteString("\n4. Open Issues / Errors\n- None recorded in fallback summary\n")
	sb.WriteString("\n5. Next Steps\n- Continue with the active user request in the retained recent turns.\n")
	return sb.String()
}
