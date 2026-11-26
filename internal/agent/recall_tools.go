package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"gp/internal/core/session"
	"gp/internal/tools"
)

const defaultRecallMaxBytes = 32 * 1024

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
			b.WriteString(fmt.Sprintf("[%s] 未找到对应消息条目\n", id))
			continue
		}
		m := entry.Message
		b.WriteString(fmt.Sprintf("=== [%s#%s", m.Role, entry.ID))
		if m.ToolCallID != "" {
			b.WriteString(fmt.Sprintf(" call_id=%s", m.ToolCallID))
		}
		b.WriteString("] ===\n")
		if m.Content != "" {
			b.WriteString(m.Content)
			if !strings.HasSuffix(m.Content, "\n") {
				b.WriteByte('\n')
			}
		}
		for _, tc := range m.ToolCalls {
			b.WriteString(fmt.Sprintf("-> tool_call %s(%s) [id=%s]\n", tc.Function.Name, string(tc.Function.Arguments), tc.ID))
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
