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
	"gp/internal/core/retrieval"
	"gp/internal/core/session"
	"gp/internal/core/storage"
)

const (
	contextLedgerDocKey          = "context_ledger"
	workingSetDocKey             = "working_set_cache"
	workingSetSchemaVersion      = 2
	strategyModeE4APR1           = "e4a_pr1"
	strategyModeE4APR2           = "e4a_pr2"
	defaultWorkingSetBudgetRatio = 0.08
	defaultWorkingSetMaxTokens   = 3600
	defaultWorkingSetFloorRatio  = 0.65
)

type assemblyMeta struct {
	strategyMode         string
	epochID              int
	epochMaskedCount     int
	ledgerVersion        int
	ledgerActiveCnt      int
	ledgerSuperseded     int
	ledgerHash           string
	workingSetTokens     int64
	workingSetEntries    int
	retrievalBuildMs     float64
	graphMatchedEntities int
	contextBuildMs       float64
	sysTokens            int64
	toolsTokens          int64
	ledgerTokens         int64
	msgsTokens           int64
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

// SetWorkingSetEnabled 开启或关闭 PR #2 的 Epoch 冻结 [WORKING_SET] 主动注入。
func (a *AgentInstance) SetWorkingSetEnabled(enabled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.workingSetEnabled = enabled
}

// GetWorkingSetEnabled 返回当前是否启用 [WORKING_SET] 主动注入。
func (a *AgentInstance) GetWorkingSetEnabled() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.workingSetEnabled
}

// SetWorkingSetBudget 配置 WorkingSet 的上下文窗口预算占比、上限 token 数与保底比例。
func (a *AgentInstance) SetWorkingSetBudget(ratio float64, maxTokens int, floorRatio float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ratio > 0 && ratio <= 1.0 {
		a.workingSetBudgetRatio = ratio
	}
	if maxTokens > 0 {
		a.workingSetMaxTokens = maxTokens
	}
	if floorRatio > 0 && floorRatio <= 1.0 {
		a.workingSetFloorRatio = floorRatio
	}
}

func (a *AgentInstance) setLastToolsTokens(toks int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastToolsTokens = toks
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
		// 自动压缩与手动压缩完成后立即同步最新压缩次数到底部状态栏
		a.EmitEvent(AgentEvent{
			Type:            EventContextMetrics,
			AgentID:         a.id,
			CompactionCount: st.CompactionCount(),
			Timestamp:       time.Now(),
		})
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

// assembleModelMessages 按 E4-A 确定性前缀顺序组装请求消息：
// [System (cached)] -> [Canonical ContextLedger (cached)] -> [Epoch Frozen WorkingSet (cached)] -> [Epoch Append-Only 近期窗口]。
func (a *AgentInstance) assembleModelMessages() []ai.Message {
	msgs, _ := a.assembleModelMessagesWithMetrics()
	return msgs
}

type wsConfigSnapshot struct {
	contextWindow    int64
	compactThreshold float64
	budgetRatio      float64
	maxTokens        int
	floorRatio       float64
	toolsTokens      int64
}

type workingSetSnapshot struct {
	SchemaVersion   int    `json:"schema_version"`
	Initialized     bool   `json:"initialized"`
	EpochID         int    `json:"epoch_id"`
	AnchorID        string `json:"anchor_id"`
	UserEntryID     string `json:"user_entry_id"`
	Text            string `json:"text"`
	Tokens          int64  `json:"tokens"`
	Entries         int    `json:"entries"`
	MatchedEntities int    `json:"matched_entities"`
}

func (a *AgentInstance) assembleModelMessagesWithMetrics() ([]ai.Message, assemblyMeta) {
	start := time.Now()

	a.mu.RLock()
	st := a.sessionTree
	sp := a.systemPrompt
	obsWindow := a.observationMaskWindow
	obsEpoch := a.observationMaskEpoch
	ledgerSnap := a.contextLedger.Clone()
	wsEnabled := a.workingSetEnabled
	wsCfg := wsConfigSnapshot{
		contextWindow:    a.contextWindow,
		compactThreshold: a.autoCompactThreshold,
		budgetRatio:      a.workingSetBudgetRatio,
		maxTokens:        a.workingSetMaxTokens,
		floorRatio:       a.workingSetFloorRatio,
		toolsTokens:      a.lastToolsTokens,
	}
	if wsCfg.contextWindow <= 0 {
		wsCfg.contextWindow = compaction.ResolveContextWindow(a.modelName)
	}
	cachedWS := workingSetSnapshot{
		Initialized:     a.lastWSInitialized,
		EpochID:         a.lastWSEpoch,
		AnchorID:        a.lastWSAnchorID,
		UserEntryID:     a.lastWSUserEntryID,
		Text:            a.lastWSText,
		Tokens:          a.lastWSTokens,
		Entries:         a.lastWSEntries,
		MatchedEntities: a.lastWSMatchedEntities,
	}
	a.mu.RUnlock()

	ver, activeCnt, supCnt, hash8 := ledgerSnap.Stats()
	ledgerText := ledgerSnap.CanonicalRender()
	mode := strategyModeE4APR2
	if !wsEnabled {
		mode = strategyModeE4APR1
	}
	meta := assemblyMeta{
		strategyMode:     mode,
		ledgerVersion:    ver,
		ledgerActiveCnt:  activeCnt,
		ledgerSuperseded: supCnt,
		ledgerHash:       hash8,
	}

	if st == nil {
		meta.contextBuildMs = float64(time.Since(start).Microseconds()) / 1000.0
		return nil, meta
	}

	linearEntries, priorToolTurns, anchorID := st.GetLinearState()
	effectiveSys := sp
	foundExistingSys := false
	window := make([]ai.Message, 0, len(linearEntries))
	windowIndices := make([]int, 0, len(linearEntries))
	for i, le := range linearEntries {
		if le.Message.Role == ai.RoleSystem {
			// 与 TC_AGT_002 保持一致：若历史中已存在 RoleSystem，沿用首条有效系统提示词并剥离，由顶部统一去重注入。
			if !foundExistingSys && strings.TrimSpace(le.Message.Content) != "" {
				effectiveSys = le.Message.Content
				foundExistingSys = true
			}
			continue
		}
		window = append(window, le.Message)
		windowIndices = append(windowIndices, i)
	}

	maskedWindow, epochID, _, maskedCount, _ := ApplyEpochObservationMaskingWithOffset(window, obsEpoch, obsWindow, priorToolTurns)
	meta.epochID = epochID
	meta.epochMaskedCount = maskedCount
	meta.sysTokens = compaction.EstimateTextTokens(effectiveSys)
	meta.ledgerTokens = compaction.EstimateTextTokens(ledgerText)
	meta.msgsTokens = compaction.EstimateMessagesTokens(maskedWindow)

	var wsText string
	if wsEnabled && (maskedCount > 0 || anchorID != "") {
		if cachedWS.Initialized && cachedWS.EpochID == epochID && cachedWS.AnchorID == anchorID {
			wsText = cachedWS.Text
			meta.workingSetTokens = cachedWS.Tokens
			meta.workingSetEntries = cachedWS.Entries
			meta.graphMatchedEntities = cachedWS.MatchedEntities
		} else {
			branchEntries := st.GetBranchEntries()
			queryText, queryEntryID := latestUserQuery(branchEntries)
			snap := workingSetSnapshot{
				SchemaVersion: workingSetSchemaVersion,
				Initialized:   true,
				EpochID:       epochID,
				AnchorID:      anchorID,
				UserEntryID:   queryEntryID,
			}
			if queryText != "" {
				_, verbatimSet := classifyBranchEntries(branchEntries, linearEntries, windowIndices, maskedWindow, anchorID)
				excludeSet := unionEntrySets(verbatimSet, ledgerSnap.SupersededEntryIDs())
				fixedTokens := meta.sysTokens + wsCfg.toolsTokens + meta.ledgerTokens + meta.msgsTokens
				wsBudget := computeWorkingSetBudget(wsCfg, fixedTokens)

				res := a.selectWorkingSet(branchEntries, queryText, wsBudget, excludeSet)
				wsRendered, wsCount, wsToks := formatWorkingSet(res.Hits)
				snap.Text = wsRendered
				snap.Tokens = wsToks
				snap.Entries = wsCount
				snap.MatchedEntities = res.MatchedEntityCount
				meta.retrievalBuildMs = res.BuildMs
			}

			wsText = snap.Text
			meta.workingSetTokens = snap.Tokens
			meta.workingSetEntries = snap.Entries
			meta.graphMatchedEntities = snap.MatchedEntities

			a.mu.Lock()
			a.lastWSInitialized = snap.Initialized
			a.lastWSEpoch = snap.EpochID
			a.lastWSAnchorID = snap.AnchorID
			a.lastWSUserEntryID = snap.UserEntryID
			a.lastWSText = snap.Text
			a.lastWSTokens = snap.Tokens
			a.lastWSEntries = snap.Entries
			a.lastWSMatchedEntities = snap.MatchedEntities
			a.mu.Unlock()
			a.persistWorkingSetSnapshot(context.Background(), snap)
		}
	}

	out := make([]ai.Message, 0, len(maskedWindow)+3)
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
	if wsText != "" {
		out = append(out, ai.Message{
			Role:         ai.RoleUser,
			Content:      wsText,
			CacheControl: true,
		})
	}
	out = append(out, maskedWindow...)
	meta.contextBuildMs = float64(time.Since(start).Microseconds()) / 1000.0
	return out, meta
}

func (a *AgentInstance) ensureRetrievalIndexSyncLocked(branchEntries []session.LinearEntry) (float64, *retrieval.Index) {
	syncStart := time.Now()
	if a.retrievalIdx == nil {
		a.retrievalIdx = retrieval.NewIndex(retrieval.DefaultConfig())
	}
	a.retrievalIdx.Sync(branchEntries)
	return float64(time.Since(syncStart).Microseconds()) / 1000.0, a.retrievalIdx
}

func (a *AgentInstance) selectWorkingSet(branchEntries []session.LinearEntry, query string, wsBudget int, excludeSet map[string]struct{}) retrieval.QueryResult {
	a.retrievalMu.Lock()
	defer a.retrievalMu.Unlock()
	syncMs, idx := a.ensureRetrievalIndexSyncLocked(branchEntries)
	res := idx.SelectWorkingSet(query, wsBudget, excludeSet)
	res.BuildMs += syncMs
	return res
}

func (a *AgentInstance) searchContextEntries(branchEntries []session.LinearEntry, query string, topK int, excludeSet map[string]struct{}) retrieval.QueryResult {
	a.retrievalMu.Lock()
	defer a.retrievalMu.Unlock()
	syncMs, idx := a.ensureRetrievalIndexSyncLocked(branchEntries)
	res := idx.SearchTopK(query, topK, excludeSet)
	res.BuildMs += syncMs
	return res
}

func (a *AgentInstance) loadPersistedWorkingSet() {
	a.mu.RLock()
	store := a.store
	sessID := a.sessionIDLocked()
	a.mu.RUnlock()

	if store == nil || sessID == "" {
		return
	}
	raw, ok, err := store.GetDocument(sessID, workingSetDocKey)
	if err != nil || !ok || len(raw) == 0 {
		return
	}
	var snap workingSetSnapshot
	if json.Unmarshal(raw, &snap) != nil || !snap.Initialized || snap.SchemaVersion != workingSetSchemaVersion {
		return
	}
	a.mu.Lock()
	a.lastWSInitialized = snap.Initialized
	a.lastWSEpoch = snap.EpochID
	a.lastWSAnchorID = snap.AnchorID
	a.lastWSUserEntryID = snap.UserEntryID
	a.lastWSText = snap.Text
	a.lastWSTokens = snap.Tokens
	a.lastWSEntries = snap.Entries
	a.lastWSMatchedEntities = snap.MatchedEntities
	a.mu.Unlock()
}

func (a *AgentInstance) persistWorkingSetSnapshot(ctx context.Context, snap workingSetSnapshot) {
	a.mu.RLock()
	store := a.store
	sessID := a.sessionIDLocked()
	a.mu.RUnlock()

	if store == nil || sessID == "" {
		return
	}
	data, err := json.Marshal(snap)
	if err != nil || len(data) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_ = store.Commit(ctx, func(tx storage.Transaction) error {
		return tx.SetDocument(sessID, workingSetDocKey, data)
	})
}

func computeWorkingSetBudget(cfg wsConfigSnapshot, fixedTokens int64) int {
	cw := cfg.contextWindow
	if cw <= 0 {
		cw = compaction.DefaultContextWindow
	}
	maxBudget := int(float64(cw) * cfg.budgetRatio)
	if cfg.maxTokens > 0 && (maxBudget <= 0 || maxBudget > cfg.maxTokens) {
		maxBudget = cfg.maxTokens
	}
	if maxBudget <= 0 {
		maxBudget = defaultWorkingSetMaxTokens
	}
	floor := int(float64(maxBudget) * cfg.floorRatio)
	if floor <= 0 {
		floor = 512
	}
	limit := cw
	if cfg.compactThreshold > 0 {
		limit = int64(float64(cw) * cfg.compactThreshold)
	}
	avail := int(limit - fixedTokens)
	budget := maxBudget
	if avail > 0 && avail < budget {
		budget = avail
	}
	if budget < floor {
		budget = floor
	}
	return budget
}

func formatWorkingSet(hits []retrieval.SearchResult) (string, int, int64) {
	if len(hits) == 0 {
		return "", 0, 0
	}
	var b strings.Builder
	b.WriteString("<working_set>\n")
	for _, h := range hits {
		msg := h.Entry.Message
		if h.Rendered == "" {
			continue
		}
		if msg.ToolCallID != "" {
			fmt.Fprintf(&b, "[%s#%s call_id=%s]: %s\n", msg.Role, h.Entry.EntryID, msg.ToolCallID, h.Rendered)
		} else {
			fmt.Fprintf(&b, "[%s#%s]: %s\n", msg.Role, h.Entry.EntryID, h.Rendered)
		}
	}
	b.WriteString("</working_set>")
	text := b.String()
	return text, len(hits), compaction.EstimateTextTokens(text)
}

func latestUserQuery(branchEntries []session.LinearEntry) (queryText, entryID string) {
	for i := len(branchEntries) - 1; i >= 0; i-- {
		if branchEntries[i].Message.Role == ai.RoleUser {
			stripped := retrieval.StripUserPromptBoilerplate(branchEntries[i].Message.Content)
			if txt := strings.TrimSpace(stripped); txt != "" {
				return txt, branchEntries[i].EntryID
			}
		}
	}
	return "", ""
}

func isCompactedToolPlaceholder(content string) bool {
	return strings.HasPrefix(content, "[已折叠历史工具输出:") || strings.HasPrefix(content, "[compacted tool output:")
}

// classifyBranchEntries aligns the masked active window back to lossless branch entries and classifies
// every branch entry as "verbatim", "masked", or "compacted". Entries in verbatimSet are present
// untruncated in maskedWindow and excluded from WorkingSet recall.
func classifyBranchEntries(
	branchEntries []session.LinearEntry,
	linearEntries []session.LinearEntry,
	windowIndices []int,
	maskedWindow []ai.Message,
	compactionAnchorID string,
) (statusByID map[string]string, verbatimSet map[string]struct{}) {
	statusByID = make(map[string]string, len(branchEntries))
	verbatimSet = make(map[string]struct{}, len(windowIndices))

	branchIdxByID := make(map[string]int, len(branchEntries))
	for i, be := range branchEntries {
		if be.EntryID != "" {
			branchIdxByID[be.EntryID] = i
		}
	}

	// Determine where pre-anchor compacted entries end in branchEntries by locating the first post-anchor EntryID.
	preAnchorEnd := len(branchEntries)
	if compactionAnchorID != "" {
		for _, idx := range windowIndices {
			le := linearEntries[idx]
			if le.EntryID != "" && le.EntryID != compactionAnchorID {
				if bi, ok := branchIdxByID[le.EntryID]; ok {
					preAnchorEnd = bi
					break
				}
			}
		}
	}

	matchedBranch := make([]bool, len(branchEntries))
	for j, srcIdx := range windowIndices {
		le := linearEntries[srcIdx]
		if le.EntryID == "" {
			continue
		}
		if compactionAnchorID != "" && le.Message.Role == ai.RoleUser && strings.HasPrefix(le.Message.Content, "<compacted_context>\n") {
			continue
		}

		isMasked := maskedWindow[j].Content != le.Message.Content || isCompactedToolPlaceholder(maskedWindow[j].Content)
		branchIdx := -1
		if compactionAnchorID == "" || le.EntryID != compactionAnchorID {
			if bi, ok := branchIdxByID[le.EntryID]; ok {
				branchIdx = bi
			}
		} else {
			for k := preAnchorEnd - 1; k >= 0; k-- {
				if matchedBranch[k] {
					continue
				}
				bm := branchEntries[k].Message
				if bm.Role != le.Message.Role || bm.ToolCallID != le.Message.ToolCallID {
					continue
				}
				if bm.Content == le.Message.Content || (isMasked && bm.Role == ai.RoleTool && bm.ToolCallID != "") {
					branchIdx = k
					break
				}
			}
		}
		if branchIdx < 0 {
			continue
		}
		matchedBranch[branchIdx] = true
		eid := branchEntries[branchIdx].EntryID
		if isMasked {
			statusByID[eid] = "masked"
		} else {
			statusByID[eid] = "verbatim"
			verbatimSet[eid] = struct{}{}
		}
	}

	for _, be := range branchEntries {
		if _, ok := statusByID[be.EntryID]; !ok {
			statusByID[be.EntryID] = "compacted"
		}
	}
	return statusByID, verbatimSet
}

func unionEntrySets(a, b map[string]struct{}) map[string]struct{} {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
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
	GetLinearContextWithToolOffset() ([]ai.Message, int)
}) int {
	if st == nil {
		return 0
	}
	a.mu.RLock()
	obsWindow := a.observationMaskWindow
	obsEpoch := a.observationMaskEpoch
	a.mu.RUnlock()

	raw, priorToolTurns := st.GetLinearContextWithToolOffset()
	window := make([]ai.Message, 0, len(raw))
	for _, m := range raw {
		if m.Role == ai.RoleSystem {
			continue
		}
		window = append(window, m)
	}
	_, epochID, _, _, _ := ApplyEpochObservationMaskingWithOffset(window, obsEpoch, obsWindow, priorToolTurns)
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
	recallCalls, restoreCalls, searchCalls int,
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
		SessionID:            sessID,
		ChannelID:            channelID,
		TurnIndex:            turnIndex,
		StrategyMode:         meta.strategyMode,
		EpochID:              meta.epochID,
		EpochMaskedCount:     meta.epochMaskedCount,
		LedgerVersion:        meta.ledgerVersion,
		LedgerActiveCnt:      meta.ledgerActiveCnt,
		LedgerSuperseded:     meta.ledgerSuperseded,
		LedgerHash:           meta.ledgerHash,
		WorkingSetTokens:     meta.workingSetTokens,
		WorkingSetEntries:    meta.workingSetEntries,
		RetrievalBuildMs:     meta.retrievalBuildMs,
		GraphMatchedEntities: meta.graphMatchedEntities,
		ContextBuildMs:       meta.contextBuildMs,
		InputTokens:          inTok,
		CacheReadTokens:      cacheReadTok,
		CacheWriteTokens:     cacheWriteTok,
		OutputTokens:         outTok,
		RecallCalls:          recallCalls,
		RestoreCalls:         restoreCalls,
		SearchCalls:          searchCalls,
		CreatedAt:            time.Now(),
	}

	var totalsPtr *storage.ContextMetricTotals
	if metricStore, ok := store.(storage.ContextMetricStore); ok && sessID != "" {
		recCtx := ctx
		if recCtx == nil || recCtx.Err() != nil {
			recCtx = context.Background()
		}
		_ = metricStore.RecordContextMetric(recCtx, metric)
		if totals, tErr := metricStore.GetContextMetricTotals(sessID); tErr == nil {
			totalsPtr = &totals
		}
	}

	a.mu.RLock()
	st := a.sessionTree
	a.mu.RUnlock()
	compactionCnt := 0
	if st != nil {
		compactionCnt = st.CompactionCount()
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
		"working_set_tokens", metric.WorkingSetTokens,
		"working_set_entries", metric.WorkingSetEntries,
		"retrieval_build_ms", metric.RetrievalBuildMs,
		"graph_matched_entities", metric.GraphMatchedEntities,
		"context_build_ms", metric.ContextBuildMs,
		"sys_tokens", meta.sysTokens,
		"tools_tokens", meta.toolsTokens,
		"ledger_tokens", meta.ledgerTokens,
		"msgs_tokens", meta.msgsTokens,
		"est_total_tokens", meta.sysTokens+meta.toolsTokens+meta.ledgerTokens+meta.workingSetTokens+meta.msgsTokens,
		"input_tokens", metric.InputTokens,
		"cache_read_tokens", metric.CacheReadTokens,
		"cache_write_tokens", metric.CacheWriteTokens,
		"output_tokens", metric.OutputTokens,
		"recall_calls", metric.RecallCalls,
		"restore_calls", metric.RestoreCalls,
		"search_calls", metric.SearchCalls,
	)

	a.EmitEvent(AgentEvent{
		Type:            EventContextMetrics,
		AgentID:         a.id,
		Turn:            turnIndex,
		ContextMetric:   metric,
		ContextTotals:   totalsPtr,
		CompactionCount: compactionCnt,
		Timestamp:       metric.CreatedAt,
	})
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
