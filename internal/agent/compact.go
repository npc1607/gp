package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gp/internal/ai"
	"gp/internal/core/compaction"
	"gp/internal/core/storage"
)

const (
	contextLedgerDocKey = "context_ledger"
	strategyModeE4APR1  = "e4a_pr1"
)

type assemblyMeta struct {
	strategyMode     string
	epochID          int
	epochMaskedCount int
	ledgerVersion    int
	ledgerActiveCnt  int
	ledgerSuperseded int
	ledgerHash       string
	contextBuildMs   float64
	sysTokens        int64
	toolsTokens      int64
	ledgerTokens     int64
	msgsTokens       int64
}

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

// ApplyLedgerEvents 增量应用一组 ContextLedger 变更事件，落盘并打印 [ledger_diff] 审计日志。
func (a *AgentInstance) ApplyLedgerEvents(ctx context.Context, events []compaction.LedgerEvent) []compaction.LedgerDiff {
	if len(events) == 0 {
		return nil
	}
	a.mu.Lock()
	if a.contextLedger == nil {
		a.contextLedger = compaction.NewContextLedger()
	}
	diffs := a.contextLedger.ApplyEventsWithDiff(events)
	ledgerSnap := a.contextLedger.Clone()
	sessID := a.sessionIDLocked()
	channelID := a.channelID
	a.mu.Unlock()

	if len(diffs) > 0 {
		a.persistContextLedger(ctx)
		logLedgerDiffs(sessID, channelID, ledgerSnap, diffs)
	}
	return diffs
}

// Compact 对当前 SessionTree 的线性上下文执行两级混合压缩，使用 LLM 结构化增量更新 ContextLedger，并追加 EntryCompaction 锚点。
func (a *AgentInstance) Compact(ctx context.Context, focus string) (*compaction.Result, error) {
	a.mu.RLock()
	st := a.sessionTree
	prov := a.provider
	modelName := a.modelName
	curLedger := a.contextLedger.Clone()
	sessID := a.sessionIDLocked()
	channelID := a.channelID
	a.mu.RUnlock()

	if st == nil {
		return nil, fmt.Errorf("compact: session tree is nil")
	}

	linearEntries := st.GetLinearEntries()
	if len(linearEntries) == 0 {
		return &compaction.Result{Ledger: curLedger}, nil
	}

	entries := make([]compaction.EntryRecord, len(linearEntries))
	for i, le := range linearEntries {
		entries[i] = compaction.EntryRecord{
			EntryID: le.EntryID,
			Message: le.Message,
		}
	}

	prevStatus := a.GetStatus()
	a.SetStatus(StatusCompacting)
	defer a.SetStatus(prevStatus)

	epochID := a.currentEpochIDLocked(st)
	freezeLedger := !curLedger.CanUpdateInEpoch(epochID)
	res, err := compaction.CompactWithEntries(ctx, prov, modelName, entries, compaction.Options{
		KeepRecentTurns:    4,
		KeepRecentMessages: 6,
		Focus:              focus,
		Ledger:             curLedger,
		CurrentEpoch:       &epochID,
		FreezeLedger:       freezeLedger,
	})
	if err != nil {
		return nil, err
	}

	if res.Ledger != nil {
		a.mu.Lock()
		a.contextLedger = res.Ledger.Clone()
		a.mu.Unlock()
		a.persistContextLedgerDoc(ctx, len(res.LedgerDiffs) > 0 || !freezeLedger)
		if len(res.LedgerDiffs) > 0 {
			logLedgerDiffs(sessID, channelID, res.Ledger, res.LedgerDiffs)
		}
	}

	if res.Summary != "" {
		st.AppendCompaction(res.Summary, res.KeptMessages)
	}
	return res, nil
}

func logLedgerDiffs(sessID, channelID string, ledger *compaction.ContextLedger, diffs []compaction.LedgerDiff) {
	ver, activeCnt, supCnt, hash8 := ledger.Stats()
	for _, d := range diffs {
		slog.Info("[ledger_diff]",
			"session_id", sessID,
			"channel_id", channelID,
			"action", d.Action,
			"constraint_id", d.ID,
			"category", d.Category,
			"supersedes_id", d.SupersedesID,
			"provenance", strings.Join(d.Provenance, ","),
			"statement", d.Statement,
			"ledger_version", ver,
			"ledger_active_cnt", activeCnt,
			"ledger_superseded", supCnt,
			"ledger_hash", hash8,
		)
	}
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

// assembleModelMessages 按 E4-A PR #1 确定性前缀顺序组装请求消息：
// [System (cached)] -> [Canonical ContextLedger (cached)] -> [Epoch Append-Only 近期窗口]。
func (a *AgentInstance) assembleModelMessages() []ai.Message {
	msgs, _ := a.assembleModelMessagesWithMetrics()
	return msgs
}

func (a *AgentInstance) assembleModelMessagesWithMetrics() ([]ai.Message, assemblyMeta) {
	start := time.Now()

	a.mu.RLock()
	st := a.sessionTree
	sp := a.systemPrompt
	obsWindow := a.observationMaskWindow
	obsEpoch := a.observationMaskEpoch
	ledgerSnap := a.contextLedger.Clone()
	a.mu.RUnlock()

	ver, activeCnt, supCnt, hash8 := ledgerSnap.Stats()
	ledgerText := ledgerSnap.CanonicalRender()
	meta := assemblyMeta{
		strategyMode:     strategyModeE4APR1,
		ledgerVersion:    ver,
		ledgerActiveCnt:  activeCnt,
		ledgerSuperseded: supCnt,
		ledgerHash:       hash8,
	}

	if st == nil {
		meta.contextBuildMs = float64(time.Since(start).Microseconds()) / 1000.0
		return nil, meta
	}

	raw, priorOffset := st.GetLinearContextWithOffset()
	effectiveSys := sp
	foundExistingSys := false
	strippedSys := 0
	window := make([]ai.Message, 0, len(raw))
	for _, m := range raw {
		if m.Role == ai.RoleSystem {
			// 与 TC_AGT_002 保持一致：若历史中已存在 RoleSystem，沿用首条有效系统提示词并剥离，由顶部统一去重注入。
			if !foundExistingSys && strings.TrimSpace(m.Content) != "" {
				effectiveSys = m.Content
				foundExistingSys = true
			}
			strippedSys++
			continue
		}
		window = append(window, m)
	}

	maskedWindow, epochID, _, maskedCount, _ := ApplyEpochObservationMaskingWithOffset(window, obsEpoch, obsWindow, priorOffset+strippedSys)
	meta.epochID = epochID
	meta.epochMaskedCount = maskedCount
	meta.sysTokens = compaction.EstimateTextTokens(effectiveSys)
	meta.ledgerTokens = compaction.EstimateTextTokens(ledgerText)
	meta.msgsTokens = compaction.EstimateMessagesTokens(maskedWindow)

	out := make([]ai.Message, 0, len(maskedWindow)+2)
	if strings.TrimSpace(effectiveSys) != "" {
		sysMsg := ai.NewSystemMessage(effectiveSys)
		sysMsg.CacheControl = true
		out = append(out, sysMsg)
	}
	if ledgerText != "" {
		out = append(out, ai.Message{
			Role:         ai.RoleUser,
			Content:      ledgerText,
			CacheControl: true,
		})
	}
	out = append(out, maskedWindow...)
	meta.contextBuildMs = float64(time.Since(start).Microseconds()) / 1000.0
	return out, meta
}

func (a *AgentInstance) sessionIDLocked() string {
	if a.sessionTree != nil && a.sessionTree.SessionID() != "" {
		return a.sessionTree.SessionID()
	}
	return a.id
}

func (a *AgentInstance) loadPersistedContextLedger() {
	a.mu.RLock()
	store := a.store
	sessID := a.sessionIDLocked()
	a.mu.RUnlock()

	if store == nil || sessID == "" {
		return
	}
	raw, ok, err := store.GetDocument(sessID, contextLedgerDocKey)
	if err != nil || !ok || len(raw) == 0 {
		return
	}
	loaded, err := compaction.UnmarshalContextLedger(raw)
	if err != nil || loaded == nil {
		return
	}

	a.mu.Lock()
	a.contextLedger = loaded
	a.lastPersistedLedgerVer = loaded.Version
	a.mu.Unlock()
}

func (a *AgentInstance) persistContextLedger(ctx context.Context) {
	a.persistContextLedgerDoc(ctx, false)
}

func (a *AgentInstance) persistContextLedgerDoc(ctx context.Context, force bool) {
	a.mu.Lock()
	store := a.store
	ledger := a.contextLedger
	if store == nil || ledger == nil || (!force && ledger.Version <= a.lastPersistedLedgerVer) {
		a.mu.Unlock()
		return
	}
	data, err := json.Marshal(ledger)
	if err != nil || len(data) == 0 {
		a.mu.Unlock()
		return
	}
	sessID := a.sessionIDLocked()
	a.lastPersistedLedgerVer = ledger.Version
	a.mu.Unlock()

	if ctx == nil {
		ctx = context.Background()
	}
	_ = store.Commit(ctx, func(tx storage.Transaction) error {
		return tx.SetDocument(sessID, contextLedgerDocKey, data)
	})
}

func (a *AgentInstance) currentEpochIDLocked(st interface {
	GetLinearContextWithOffset() ([]ai.Message, int)
}) int {
	if st == nil {
		return 0
	}
	a.mu.RLock()
	obsWindow := a.observationMaskWindow
	obsEpoch := a.observationMaskEpoch
	a.mu.RUnlock()

	raw, priorOffset := st.GetLinearContextWithOffset()
	stripped := 0
	window := make([]ai.Message, 0, len(raw))
	for _, m := range raw {
		if m.Role == ai.RoleSystem {
			stripped++
			continue
		}
		window = append(window, m)
	}
	_, epochID, _, _, _ := ApplyEpochObservationMaskingWithOffset(window, obsEpoch, obsWindow, priorOffset+stripped)
	return epochID
}

func (a *AgentInstance) nextMetricTurnBase() int {
	a.mu.RLock()
	store := a.store
	sessID := a.sessionIDLocked()
	a.mu.RUnlock()

	if metricStore, ok := store.(storage.ContextMetricStore); ok && sessID != "" {
		if totals, err := metricStore.GetContextMetricTotals(sessID); err == nil {
			return totals.TurnCount
		}
	}
	return 0
}

func (a *AgentInstance) recordTurnContextMetric(
	ctx context.Context,
	turnIndex int,
	meta assemblyMeta,
	usage *ai.TokenUsage,
	recallCalls, restoreCalls int,
) {
	if a.id == "risk_guard" {
		return
	}

	a.mu.RLock()
	store := a.store
	sessID := a.sessionIDLocked()
	channelID := a.channelID
	a.mu.RUnlock()

	var inTok, cacheReadTok, cacheWriteTok, outTok int64
	if usage != nil {
		inTok = usage.InputTokens
		cacheReadTok = usage.CacheReadTokens
		cacheWriteTok = usage.CacheWriteTokens
		outTok = usage.OutputTokens
	}

	metric := &storage.ContextMetric{
		SessionID:        sessID,
		ChannelID:        channelID,
		TurnIndex:        turnIndex,
		StrategyMode:     meta.strategyMode,
		EpochID:          meta.epochID,
		EpochMaskedCount: meta.epochMaskedCount,
		LedgerVersion:    meta.ledgerVersion,
		LedgerActiveCnt:  meta.ledgerActiveCnt,
		LedgerSuperseded: meta.ledgerSuperseded,
		LedgerHash:       meta.ledgerHash,
		ContextBuildMs:   meta.contextBuildMs,
		InputTokens:      inTok,
		CacheReadTokens:  cacheReadTok,
		CacheWriteTokens: cacheWriteTok,
		OutputTokens:     outTok,
		RecallCalls:      recallCalls,
		RestoreCalls:     restoreCalls,
		CreatedAt:        time.Now(),
	}

	if metricStore, ok := store.(storage.ContextMetricStore); ok && sessID != "" {
		recCtx := ctx
		if recCtx == nil || recCtx.Err() != nil {
			recCtx = context.Background()
		}
		_ = metricStore.RecordContextMetric(recCtx, metric)
	}

	slog.Info("[ctx_metrics]",
		"session_id", metric.SessionID,
		"channel_id", metric.ChannelID,
		"turn_index", metric.TurnIndex,
		"strategy_mode", metric.StrategyMode,
		"epoch_id", metric.EpochID,
		"epoch_masked_count", metric.EpochMaskedCount,
		"ledger_version", metric.LedgerVersion,
		"ledger_active_cnt", metric.LedgerActiveCnt,
		"ledger_superseded", metric.LedgerSuperseded,
		"ledger_hash", metric.LedgerHash,
		"context_build_ms", metric.ContextBuildMs,
		"sys_tokens", meta.sysTokens,
		"tools_tokens", meta.toolsTokens,
		"ledger_tokens", meta.ledgerTokens,
		"msgs_tokens", meta.msgsTokens,
		"est_total_tokens", meta.sysTokens+meta.toolsTokens+meta.ledgerTokens+meta.msgsTokens,
		"input_tokens", metric.InputTokens,
		"cache_read_tokens", metric.CacheReadTokens,
		"cache_write_tokens", metric.CacheWriteTokens,
		"output_tokens", metric.OutputTokens,
		"recall_calls", metric.RecallCalls,
		"restore_calls", metric.RestoreCalls,
	)
}

func (a *AgentInstance) maybeAutoCompact(ctx context.Context, messages []ai.Message) []ai.Message {
	msgs, _, _ := a.maybeAutoCompactWithMeta(ctx, messages, 0)
	return msgs
}

func (a *AgentInstance) maybeAutoCompactWithMeta(ctx context.Context, messages []ai.Message, toolsTokens int64) ([]ai.Message, assemblyMeta, bool) {
	cw := a.GetContextWindow()
	threshold := a.GetAutoCompactThreshold()
	if cw <= 0 || threshold <= 0 || len(messages) <= 6 {
		return messages, assemblyMeta{}, false
	}
	est := compaction.EstimateMessagesTokens(messages) + toolsTokens
	limit := int64(float64(cw) * threshold)
	if est < limit {
		return messages, assemblyMeta{}, false
	}
	res, err := a.Compact(ctx, "")
	if err != nil || res == nil || (res.Summary == "" && (res.Ledger == nil || res.Ledger.CanonicalRender() == "")) {
		return messages, assemblyMeta{}, false
	}
	msgs, meta := a.assembleModelMessagesWithMetrics()
	meta.toolsTokens = toolsTokens
	return msgs, meta, true
}
