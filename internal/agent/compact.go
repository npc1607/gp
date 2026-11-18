package agent

import (
	"context"
	"fmt"

	"gp/internal/ai"
	"gp/internal/core/compaction"
)

// SetContextWindow 显式覆盖当前 Agent 的上下文窗口大小（<= 0 时按模型名自动解析）
func (a *AgentInstance) SetContextWindow(tokens int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if tokens <= 0 {
		a.contextWindow = compaction.ResolveContextWindow(a.modelName)
		return
	}
	a.contextWindow = tokens
}

// GetContextWindow 返回当前 Agent 的有效上下文窗口大小
func (a *AgentInstance) GetContextWindow() int64 {
	a.mu.RLock()
	cw := a.contextWindow
	modelName := a.modelName
	a.mu.RUnlock()
	if cw > 0 {
		return cw
	}
	return compaction.ResolveContextWindow(modelName)
}

// SetAutoCompactThreshold 设置自动压缩触发阈值比例（如 0.80 表示达到 80% 时触发；<= 0 禁用自动压缩）
func (a *AgentInstance) SetAutoCompactThreshold(ratio float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.autoCompactThreshold = ratio
}

// GetAutoCompactThreshold 返回自动压缩触发阈值比例
func (a *AgentInstance) GetAutoCompactThreshold() float64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.autoCompactThreshold
}

// Compact 对当前 SessionTree 的线性上下文执行两级混合压缩，并追加 EntryCompaction 锚点。
func (a *AgentInstance) Compact(ctx context.Context, focus string) (*compaction.Result, error) {
	a.mu.RLock()
	st := a.sessionTree
	prov := a.provider
	modelName := a.modelName
	a.mu.RUnlock()

	if st == nil {
		return nil, fmt.Errorf("compact: session tree is nil")
	}

	msgs := st.GetLinearContext()
	if len(msgs) == 0 {
		return &compaction.Result{}, nil
	}

	prevStatus := a.GetStatus()
	a.SetStatus(StatusCompacting)
	defer a.SetStatus(prevStatus)

	res, err := compaction.Compact(ctx, prov, modelName, msgs, compaction.Options{
		KeepRecentTurns:    4,
		KeepRecentMessages: 6,
		Focus:              focus,
	})
	if err != nil {
		return nil, err
	}

	if res.Summary != "" {
		st.AppendCompaction(res.Summary, res.KeptMessages)
	}
	return res, nil
}

func (a *AgentInstance) buildModelMessages() []ai.Message {
	messages := a.sessionTree.GetLinearContext()
	if sp := a.GetSystemPrompt(); sp != "" {
		hasSys := false
		for _, m := range messages {
			if m.Role == ai.RoleSystem {
				hasSys = true
				break
			}
		}
		if !hasSys {
			messages = append([]ai.Message{ai.NewSystemMessage(sp)}, messages...)
		}
	}
	return messages
}

func (a *AgentInstance) maybeAutoCompact(ctx context.Context, messages []ai.Message) []ai.Message {
	cw := a.GetContextWindow()
	threshold := a.GetAutoCompactThreshold()
	if cw <= 0 || threshold <= 0 || len(messages) <= 6 {
		return messages
	}
	est := compaction.EstimateMessagesTokens(messages)
	limit := int64(float64(cw) * threshold)
	if est < limit {
		return messages
	}
	res, err := a.Compact(ctx, "")
	if err != nil || res == nil || res.Summary == "" {
		return messages
	}
	return a.buildModelMessages()
}
