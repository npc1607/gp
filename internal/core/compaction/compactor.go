package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gp/internal/ai"
)

// EntryRecord pairs a SessionTree entry_id with its message payload for provenance-aware compaction.
type EntryRecord struct {
	EntryID string
	Message ai.Message
}

// Options 配置两级上下文压缩策略
type Options struct {
	KeepRecentTurns    int            // Stage 1 保留完整工具输出的最近用户轮次数（默认 4）
	KeepRecentMessages int            // Stage 2 保留不压缩的尾部消息最小条数（默认 6）
	Focus              string         // 可选用户指定的摘要保留重点
	Ledger             *ContextLedger // 可选既有 L1 结构化约束账本（用于增量更新）
	CurrentEpoch       *int           // 当前累计 Epoch ID（同一 Epoch 内仅允许更新一次账本）
	FreezeLedger       bool           // 为 true 时本次压缩严禁修改 ContextLedger，仅生成 handoff_notes
}

// Result 描述一次上下文压缩的执行结果
type Result struct {
	Summary         string
	KeptMessages    []ai.Message
	Ledger          *ContextLedger
	LedgerDiffs     []LedgerDiff
	HandoffNotes    string
	BeforeTokens    int64
	AfterTokens     int64
	PrunedToolCount int
	UsedLLM         bool
}

type structuredCompactResponse struct {
	LedgerEvents []LedgerEvent `json:"ledger_events"`
	HandoffNotes string        `json:"handoff_notes"`
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
		if len(out[i].Images) > 0 {
			out[i].Images = nil
			out[i].Content += "\n[compacted image attachment]"
			pruned++
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

// Compact 执行两级混合上下文压缩：先做 Stage 1 工具输出裁剪，再由 LLM 增量维护 ContextLedger 并生成精简交接信息。
func Compact(ctx context.Context, prov ai.Provider, modelName string, msgs []ai.Message, opts Options) (*Result, error) {
	entries := make([]EntryRecord, len(msgs))
	for i, m := range msgs {
		entries[i] = EntryRecord{Message: m}
	}
	return CompactWithEntries(ctx, prov, modelName, entries, opts)
}

// CompactWithEntries 在保留原始 entry_id 指针的前提下执行两级上下文压缩与 L1 ContextLedger 增量更新。
func CompactWithEntries(ctx context.Context, prov ai.Provider, modelName string, entries []EntryRecord, opts Options) (*Result, error) {
	if opts.KeepRecentTurns <= 0 {
		opts.KeepRecentTurns = 4
	}
	if opts.KeepRecentMessages <= 0 {
		opts.KeepRecentMessages = 6
	}

	msgs := make([]ai.Message, len(entries))
	for i, e := range entries {
		msgs[i] = e.Message
	}

	ledger := opts.Ledger.Clone()
	beforeTokens := EstimateMessagesTokens(msgs)
	prunedMsgs, prunedCount := PruneToolOutputs(msgs, opts.KeepRecentTurns)

	// 按 User 消息边界切分：保留尾部最近对话，不打断 tool_call / tool_result 配对
	splitIdx := findTailSplitIndex(prunedMsgs, opts.KeepRecentMessages)
	if splitIdx <= 0 {
		after := EstimateMessagesTokens(prunedMsgs)
		return &Result{
			Summary:         "",
			KeptMessages:    prunedMsgs,
			Ledger:          ledger,
			BeforeTokens:    beforeTokens,
			AfterTokens:     after,
			PrunedToolCount: prunedCount,
		}, nil
	}

	headEntries := make([]EntryRecord, splitIdx)
	for i := 0; i < splitIdx; i++ {
		headEntries[i] = EntryRecord{
			EntryID: entries[i].EntryID,
			Message: prunedMsgs[i],
		}
	}
	tail := prunedMsgs[splitIdx:]

	freezeLedger := opts.FreezeLedger
	if !freezeLedger && opts.CurrentEpoch != nil && !ledger.CanUpdateInEpoch(*opts.CurrentEpoch) {
		freezeLedger = true
	}
	handoff, diffs, usedLLM := compactHeadWithLedger(ctx, prov, modelName, headEntries, ledger, opts.Focus, freezeLedger)
	if !freezeLedger && usedLLM && opts.CurrentEpoch != nil {
		ledger.MarkEpochUpdated(*opts.CurrentEpoch)
	}
	ledgerText := ledger.CanonicalRender()
	summaryMsg := BuildSummaryMessage(handoff)
	afterTokens := EstimateTextTokens(ledgerText) + EstimateMessageTokens(summaryMsg) + EstimateMessagesTokens(tail)

	return &Result{
		Summary:         handoff,
		KeptMessages:    tail,
		Ledger:          ledger,
		LedgerDiffs:     diffs,
		HandoffNotes:    handoff,
		BeforeTokens:    beforeTokens,
		AfterTokens:     afterTokens,
		PrunedToolCount: prunedCount,
		UsedLLM:         usedLLM,
	}, nil
}

// BuildSummaryMessage 将压缩交接信息封装为注入给大模型的用户侧上下文消息
func BuildSummaryMessage(summary string) ai.Message {
	trimmed := strings.TrimSpace(summary)
	if trimmed == "" {
		return ai.Message{}
	}
	return ai.Message{
		Role:    ai.RoleUser,
		Content: "<compacted_context>\n" + trimmed + "\n</compacted_context>",
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

func compactHeadWithLedger(ctx context.Context, prov ai.Provider, modelName string, head []EntryRecord, ledger *ContextLedger, focus string, freezeLedger bool) (string, []LedgerDiff, bool) {
	if prov != nil && modelName != "" {
		if rawOut, err := callLLMStructuredCompact(ctx, prov, modelName, head, ledger, focus, freezeLedger); err == nil && strings.TrimSpace(rawOut) != "" {
			if parsed, ok := parseStructuredCompactResponse(rawOut); ok {
				var diffs []LedgerDiff
				if !freezeLedger && len(parsed.LedgerEvents) > 0 {
					diffs = ledger.ApplyEventsWithDiff(parsed.LedgerEvents)
				}
				notes := strings.TrimSpace(parsed.HandoffNotes)
				if notes == "" && ledger.CanonicalRender() != "" {
					notes = "Constraints and architectural decisions are tracked in [CONTEXT_LEDGER]; use recall_entries(entry_ids) for verbatim verification."
				}
				return notes, diffs, true
			}
			// 兼容非 JSON 格式模型响应（或单测 Mock 文本摘要）
			return strings.TrimSpace(rawOut), nil, true
		}
	}
	return buildExtractiveFallbackSummary(head, focus), nil, false
}

func callLLMStructuredCompact(ctx context.Context, prov ai.Provider, modelName string, head []EntryRecord, ledger *ContextLedger, focus string, freezeLedger bool) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var transcript strings.Builder
	for i, rec := range head {
		m := rec.Message
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
		idLabel := rec.EntryID
		if idLabel == "" {
			idLabel = fmt.Sprintf("turn_%d", i)
		}
		fmt.Fprintf(&transcript, "[%s#%s]: %s\n", m.Role, idLabel, content)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&transcript, "  -> tool_call %s(%s)\n", tc.Function.Name, string(tc.Function.Arguments))
		}
	}

	existingLedgerJSON := "{}"
	if ledger != nil && len(ledger.Constraints) > 0 {
		if b, err := json.MarshalIndent(ledger, "", "  "); err == nil {
			existingLedgerJSON = string(b)
		}
	}

	focusBlock := ""
	if strings.TrimSpace(focus) != "" {
		focusBlock = "\nAdditional focus requested by user: " + strings.TrimSpace(focus) + "\n"
	}

	ledgerRule := `Rules:
- ONLY add entries for (1) explicit hard user/project constraints or prohibitions, (2) architectural/tech-stack/approach decisions or changes, and (3) unresolved blocking issues.
- FORBIDDEN in ledger_events (put temporary working notes in handoff_notes instead if needed):
  * System prompt rules, role personas ([角色人设]), markdown/card/image reply instructions, or Feishu chat metadata (chat_id, sender_id, open_id, msg_id).
  * Routine code-reading trivia: specific function boolean formulas, field/struct names, git branch names, commit SHAs, or directory listings.
  * Redundant re-statements of constraints already active in Existing ContextLedger — emit "ledger_events": [] when nothing new matches.
- Use "supersede" whenever a later user decision invalidates or replaces an earlier constraint in Existing ContextLedger.
- Use "resolve" when an open issue or temporary constraint is completed/resolved.
- Every provenance array MUST cite exact entry_id values from the transcript brackets (e.g. [user#entry_123] -> "entry_123").
- Keep handoff_notes concise and do NOT repeat superseded decisions as active rules.`
	if freezeLedger {
		ledgerRule = `Rules:
- ContextLedger is FROZEN for the current Epoch: you MUST return "ledger_events": [] (empty array).
- Summarize completed files, findings, errors, and next steps concisely in "handoff_notes" only.`
	}

	prompt := fmt.Sprintf(`Update the structured ContextLedger incrementally and produce concise handoff_notes for the compacted conversation history.%s
Return strictly valid JSON matching this schema (no extra commentary):
{
  "ledger_events": [
    {
      "action": "add | supersede | resolve",
      "id": "stable_snake_case_id",
      "category": "tech_stack | constraint | architecture | environment | api | issue",
      "statement": "concise factual invariant or decision",
      "supersedes_id": "existing_id_if_action_is_supersede",
      "provenance": ["entry_id_from_transcript"]
    }
  ],
  "handoff_notes": "concise handoff of completed files, errors, and next steps not already captured in ledger_events"
}

%s

Existing ContextLedger:
%s

Conversation history to compact:
%s`, focusBlock, ledgerRule, existingLedgerJSON, transcript.String())

	ch, err := prov.StreamChat(ctx, &ai.ChatRequest{
		Model: modelName,
		Messages: []ai.Message{
			{Role: ai.RoleSystem, Content: "You are a precise technical context compactor. Output strict JSON with incremental ContextLedger events (citing exact entry_ids) and concise handoff_notes."},
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

func parseStructuredCompactResponse(raw string) (structuredCompactResponse, bool) {
	cleaned := strings.TrimSpace(raw)
	if strings.HasPrefix(cleaned, "```") {
		if idx := strings.IndexByte(cleaned, '\n'); idx >= 0 {
			cleaned = cleaned[idx+1:]
		}
		if end := strings.LastIndex(cleaned, "```"); end >= 0 {
			cleaned = cleaned[:end]
		}
		cleaned = strings.TrimSpace(cleaned)
	}
	start := strings.IndexByte(cleaned, '{')
	end := strings.LastIndexByte(cleaned, '}')
	if start < 0 || end <= start {
		return structuredCompactResponse{}, false
	}
	var resp structuredCompactResponse
	if err := json.Unmarshal([]byte(cleaned[start:end+1]), &resp); err != nil {
		return structuredCompactResponse{}, false
	}
	if resp.LedgerEvents == nil && strings.TrimSpace(resp.HandoffNotes) == "" {
		return structuredCompactResponse{}, false
	}
	return resp, true
}

func buildExtractiveFallbackSummary(head []EntryRecord, focus string) string {
	var sb strings.Builder
	sb.WriteString("1. Goal & Constraints\n")
	if strings.TrimSpace(focus) != "" {
		fmt.Fprintf(&sb, "- Focus: %s\n", strings.TrimSpace(focus))
	}
	userCount := 0
	for _, rec := range head {
		m := rec.Message
		if m.Role == ai.RoleUser && strings.TrimSpace(m.Content) != "" {
			line := strings.ReplaceAll(strings.TrimSpace(m.Content), "\n", " ")
			if len(line) > 160 {
				line = line[:160] + "…"
			}
			if rec.EntryID != "" {
				fmt.Fprintf(&sb, "- User [%s]: %s\n", rec.EntryID, line)
			} else {
				fmt.Fprintf(&sb, "- User: %s\n", line)
			}
			userCount++
			if userCount >= 5 {
				break
			}
		}
	}
	sb.WriteString("\n2. Key Files & Symbols\n- (Preserved from compacted history; use recall_entries if needed)\n")
	sb.WriteString("\n3. Completed Changes & Decisions\n")
	toolCalls := 0
	for _, rec := range head {
		for _, tc := range rec.Message.ToolCalls {
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
