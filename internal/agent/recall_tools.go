package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"gp/internal/ai"
	"gp/internal/core/retrieval"
	"gp/internal/core/session"
	"gp/internal/tools"
)

const (
	defaultRecallMaxBytes    = 32 * 1024
	defaultSearchContextTopK = 6
	maxSearchContextTopK     = 12
	searchPreviewRunes       = 240
)

// recallEntriesTool implements lossless L0 provenance retrieval by entry_id from SessionTree / SQLite.
type recallEntriesTool struct {
	agent *AgentInstance
}

func (t *recallEntriesTool) Name() string { return "recall_entries" }

func (t *recallEntriesTool) Description() string {
	return "按 entry_id 从当前会话的无损历史仓库（SessionTree / 存储）精确取回原始消息或工具调用记录，用于核对 ContextLedger 约束 provenance 出处或取回被压缩/替换的历史原文。"
}

func (t *recallEntriesTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"entry_ids": {
				"type": "array",
				"items": {"type": "string"},
				"description": "要回查的 entry_id 列表（例如 ContextLedger 中 [provenance:entry_...] 引用的 ID）"
			}
		},
		"required": ["entry_ids"]
	}`)
}

func (t *recallEntriesTool) ReplayMode() tools.ReplayMode {
	return tools.ReplaySafe
}

func (t *recallEntriesTool) Execute(_ *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	var args struct {
		EntryIDs []string `json:"entry_ids"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &tools.ToolResult{Content: fmt.Sprintf("参数解析失败: %v", err), IsError: true}, nil
	}
	if len(args.EntryIDs) == 0 {
		return &tools.ToolResult{Content: "缺少必填参数 entry_ids", IsError: true}, nil
	}

	t.agent.mu.RLock()
	st := t.agent.sessionTree
	store := t.agent.store
	sessID := t.agent.id
	if st != nil && st.SessionID() != "" {
		sessID = st.SessionID()
	}
	t.agent.mu.RUnlock()

	if st == nil && store == nil {
		return &tools.ToolResult{Content: "当前会话未启用 SessionTree 或持久化存储，无法回查历史条目", IsError: true}, nil
	}

	var storedIndex map[string]*session.SessionEntry
	lookupEntry := func(id string) (*session.SessionEntry, bool) {
		if st != nil {
			if e, ok := st.GetEntry(id); ok {
				return e, true
			}
		}
		if store != nil {
			if storedIndex == nil {
				storedIndex = make(map[string]*session.SessionEntry)
				if rawEntries, err := store.GetEntries(sessID); err == nil {
					for _, re := range rawEntries {
						var se session.SessionEntry
						if json.Unmarshal(re.Payload, &se) == nil {
							if se.ID == "" {
								se.ID = re.ID
							}
							storedIndex[se.ID] = &se
						}
					}
				}
			}
			if se, ok := storedIndex[id]; ok {
				return se, true
			}
		}
		return nil, false
	}

	var b strings.Builder
	validRequests := 0
	for _, rawID := range args.EntryIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		validRequests++
		entry, ok := lookupEntry(id)
		if !ok || entry.Type != session.EntryMessage || entry.Message == nil {
			fmt.Fprintf(&b, "[%s] 未找到对应消息条目\n", id)
			continue
		}
		m := entry.Message
		fmt.Fprintf(&b, "=== [%s#%s", m.Role, entry.ID)
		if m.ToolCallID != "" {
			fmt.Fprintf(&b, " call_id=%s", m.ToolCallID)
		}
		b.WriteString("] ===\n")
		if m.Content != "" {
			b.WriteString(m.Content)
			if !strings.HasSuffix(m.Content, "\n") {
				b.WriteByte('\n')
			}
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "-> tool_call %s(%s) [id=%s]\n", tc.Function.Name, string(tc.Function.Arguments), tc.ID)
		}
		if b.Len() >= defaultRecallMaxBytes {
			b.WriteString("\n...[recall_entries output reached 32KB limit; request fewer entry_ids per call]...\n")
			break
		}
	}

	if validRequests == 0 {
		return &tools.ToolResult{Content: "未提供有效的 entry_id", IsError: true}, nil
	}
	return &tools.ToolResult{Content: strings.TrimRight(b.String(), "\n")}, nil
}

// searchContextTool provides on-demand hybrid PPR + ACT-R + BM25 + MMR retrieval across full branch history.
type searchContextTool struct {
	agent *AgentInstance
}

func (t *searchContextTool) Name() string { return "search_context" }

func (t *searchContextTool) Description() string {
	return "基于混合图谱与语义检索（BM25 + Dense + PPR 实体扩散 + ACT-R + MMR），在当前会话完整分支历史（含已压缩或已折叠条目）中按相关性检索并定位历史 entry_id，返回精简预览；需查看逐字原文时再调用 recall_entries(entry_ids)。"
}

func (t *searchContextTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "检索关键词、代码符号、文件路径或自然语言问题"
			},
			"top_k": {
				"type": "integer",
				"description": "返回最大条数（默认 6，最大 12）"
			},
			"include_superseded": {
				"type": "boolean",
				"description": "是否包含已被 ContextLedger 废弃（superseded）约束关联的历史条目（默认 false）"
			}
		},
		"required": ["query"]
	}`)
}

func (t *searchContextTool) ReplayMode() tools.ReplayMode {
	return tools.ReplaySafe
}

func (t *searchContextTool) Execute(_ *tools.ToolContext, rawArgs json.RawMessage) (*tools.ToolResult, error) {
	var args struct {
		Query             string `json:"query"`
		TopK              int    `json:"top_k"`
		IncludeSuperseded bool   `json:"include_superseded"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return &tools.ToolResult{Content: fmt.Sprintf("参数解析失败: %v", err), IsError: true}, nil
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return &tools.ToolResult{Content: "缺少必填参数 query", IsError: true}, nil
	}
	topK := args.TopK
	if topK <= 0 {
		topK = defaultSearchContextTopK
	}
	if topK > maxSearchContextTopK {
		topK = maxSearchContextTopK
	}

	t.agent.mu.RLock()
	st := t.agent.sessionTree
	store := t.agent.store
	sessID := t.agent.sessionIDLocked()
	obsEpoch := t.agent.observationMaskEpoch
	obsWindow := t.agent.observationMaskWindow
	ledgerSnap := t.agent.contextLedger.Clone()
	t.agent.mu.RUnlock()

	if st == nil && store == nil {
		return &tools.ToolResult{Content: "当前会话未启用 SessionTree 或持久化存储，无法检索历史上下文", IsError: true}, nil
	}

	var branchEntries []session.LinearEntry
	if st != nil {
		branchEntries = st.GetBranchEntries()
	}
	if len(branchEntries) == 0 && store != nil && sessID != "" {
		if rawEntries, err := store.GetEntries(sessID); err == nil {
			for _, re := range rawEntries {
				if re.Type != string(session.EntryMessage) {
					continue
				}
				var se session.SessionEntry
				if json.Unmarshal(re.Payload, &se) == nil && se.Message != nil {
					id := se.ID
					if id == "" {
						id = re.ID
					}
					branchEntries = append(branchEntries, session.LinearEntry{
						EntryID: id,
						Message: *se.Message,
					})
				}
			}
		}
	}

	if len(branchEntries) == 0 {
		return &tools.ToolResult{Content: fmt.Sprintf("[search_context] query=%q 暂无可检索的历史消息", query)}, nil
	}

	statusByID := make(map[string]string, len(branchEntries))
	if st != nil {
		linearEntries, priorToolTurns, anchorID := st.GetLinearState()
		window := make([]ai.Message, 0, len(linearEntries))
		windowIndices := make([]int, 0, len(linearEntries))
		for i, le := range linearEntries {
			if le.Message.Role == ai.RoleSystem {
				continue
			}
			window = append(window, le.Message)
			windowIndices = append(windowIndices, i)
		}
		maskedWindow, _, _, _, _ := ApplyEpochObservationMaskingWithOffset(window, obsEpoch, obsWindow, priorToolTurns)
		statusByID, _ = classifyBranchEntries(branchEntries, linearEntries, windowIndices, maskedWindow, anchorID)
	}

	var excludeSet map[string]struct{}
	if !args.IncludeSuperseded && ledgerSnap != nil {
		excludeSet = ledgerSnap.SupersededEntryIDs()
	}

	res := t.agent.searchContextEntries(branchEntries, query, topK, excludeSet)
	if len(res.Hits) == 0 {
		return &tools.ToolResult{
			Content: fmt.Sprintf("[search_context] query=%q 未命中相关历史条目（graph_matched_entities=%d, %.2fms）", query, res.MatchedEntityCount, res.BuildMs),
		}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[search_context] query=%q hits=%d matched_entities=%d build_ms=%.2f\n",
		query, len(res.Hits), res.MatchedEntityCount, res.BuildMs)
	for i, hit := range res.Hits {
		status := statusByID[hit.Entry.EntryID]
		if status == "" {
			status = "compacted"
		}
		fmt.Fprintf(&b, "\n%d. [%s#%s status=%s score=%.3f",
			i+1, hit.Entry.Message.Role, hit.Entry.EntryID, status, hit.Score)
		if hit.Entry.Message.ToolCallID != "" {
			fmt.Fprintf(&b, " call_id=%s", hit.Entry.Message.ToolCallID)
		}
		if len(hit.MatchedEntities) > 0 {
			fmt.Fprintf(&b, " entities=%s", strings.Join(hit.MatchedEntities, ","))
		}
		b.WriteString("]\n")
		b.WriteString(buildSearchPreview(hit.Rendered, query, searchPreviewRunes))
		b.WriteByte('\n')
	}
	b.WriteString("\n提示：如需查看以上任意条目的完整逐字原文，请调用 recall_entries(entry_ids=[...])。")
	return &tools.ToolResult{Content: b.String()}, nil
}

func buildSearchPreview(rendered, query string, maxRunes int) string {
	cleaned := strings.TrimSpace(strings.ReplaceAll(rendered, "\r\n", "\n"))
	if cleaned == "" {
		return ""
	}
	if maxRunes <= 0 {
		maxRunes = searchPreviewRunes
	}
	runes := []rune(cleaned)
	if len(runes) <= maxRunes {
		return cleaned
	}

	// Center preview window around the strongest query token occurrence when located deep inside long payloads.
	lower := strings.ToLower(cleaned)
	bestBytePos := -1
	for _, tok := range retrieval.CodeTokenize(query) {
		if len([]rune(tok)) < 2 {
			continue
		}
		if pos := strings.Index(lower, tok); pos >= 0 {
			bestBytePos = pos
			break
		}
	}
	startRune := 0
	if bestBytePos > 0 {
		charOffset := utf8.RuneCountInString(cleaned[:bestBytePos])
		if charOffset > maxRunes/2 {
			startRune = charOffset - maxRunes/3
		}
	}
	if startRune+maxRunes > len(runes) {
		startRune = len(runes) - maxRunes
		if startRune < 0 {
			startRune = 0
		}
	}
	endRune := startRune + maxRunes
	if endRune > len(runes) {
		endRune = len(runes)
	}
	snippet := string(runes[startRune:endRune])
	if startRune > 0 {
		snippet = "…" + snippet
	}
	if endRune < len(runes) {
		snippet += "…"
	}
	return snippet
}
