package retrieval

import (
	"fmt"
	"strings"
	"testing"

	"gp/internal/ai"
	"gp/internal/core/session"
)

func TestPPR_MultiHopBridgeAndHubDegreePenalty(t *testing.T) {
	g := NewEntityGraph()
	ext := HeuristicExtractor{}
	emb := NewHashNGramEmbedder(128)

	add := func(idx int, text string) {
		msg := ai.NewAssistantMessage(text, "")
		g.AddMessage(idx, msg, ext.ExtractMessage(msg), emb.Embed(text))
	}

	// Entry 0: isolated target chained via specific proto file entity (2-hop from query).
	add(0, "proto/analytics/audit_meta.proto defines reserved 12 and auditSlotVerifier for audit integrity.")
	// Entry 1: bridges super-hub pkg/shared/hub.go and proto/analytics/audit_meta.proto.
	add(1, "internal/service/exporter.go imports proto/analytics/audit_meta.proto and calls pkg/shared/hub.go")
	// Entries 2..25: noise touching super-hub pkg/shared/hub.go only.
	for i := 2; i < 26; i++ {
		add(i, fmt.Sprintf("routine log handler %d touches pkg/shared/hub.go without proto dependency", i))
	}

	// Query mentions auditSlotVerifier (bridges 0) & internal/service/exporter.go + pkg/shared/hub.go.
	qEnts := ext.ExtractQuery("How does exporter.go interact with pkg/shared/hub.go and analytics/audit_meta.proto?")
	if got := g.CountMatchedEntities(qEnts); got == 0 {
		t.Fatalf("expected query entities to match graph, got 0 (qEnts=%v)", qEnts)
	}

	seed := g.BuildSeedDistribution(qEnts, []int{25, 24})
	rawPPR := PersonalizedPageRank(g, seed, 0.75, 20, 0.4)
	msgScores := g.ProjectMessageScores(rawPPR, 120.0)

	// Entry 0 (2-hop rare entity bridge via analytics/audit_meta.proto) must outrank arbitrary hub-only noise entries.
	if msgScores[0] <= msgScores[10] {
		t.Fatalf("expected multi-hop rare bridge entry[0] (%.4f) to outscore hub noise entry[10] (%.4f)", msgScores[0], msgScores[10])
	}

	// Check degree penalty effect: comparing beta=0.4 against beta=0 should lower relative hub-noise score ratio.
	noPen := PersonalizedPageRank(g, seed, 0.75, 20, 0.0)
	noPenScores := g.ProjectMessageScores(noPen, 120.0)
	ratioPen := msgScores[10] / msgScores[1]
	ratioNoPen := noPenScores[10] / noPenScores[1]
	if ratioPen >= ratioNoPen {
		t.Fatalf("expected degreeBeta=0.4 to dampen super-hub noise ratio vs exporter: withBeta=%.4f withoutBeta=%.4f", ratioPen, ratioNoPen)
	}
}

func TestZlibDensityPenalty_StructuredPreservedSpamPenalized(t *testing.T) {
	// Realistic structured diff/Go/JSON (> 1200 chars) should NOT trigger the harsh 0.45x repetition penalty.
	structured := strings.Join([]string{
		"diff --git a/internal/auth/oauth_config.go b/internal/auth/oauth_config.go",
		"index 4a81b92..9c30d7e 100644",
		"--- a/internal/auth/oauth_config.go",
		"+++ b/internal/auth/oauth_config.go",
		"@@ -42,18 +42,39 @@ type TokenExchangeRequest struct {",
		"+\tClientID       string   `json:\"client_id\"`",
		"+\tAudience       string   `json:\"audience\"`",
		"+\tAllowedScopes  []string `json:\"allowed_scopes\"`",
		"+\tRotationWindow int64    `json:\"rotation_window_sec\"`",
		"+}",
		"+",
		"+func ValidateTokenScope(ctx context.Context, cfg *OAuthConfig, req *TokenExchangeRequest) error {",
		"+\tif cfg == nil || req == nil {",
		"+\t\treturn errors.New(\"oauth: nil configuration or token exchange payload\")",
		"+\t}",
		"+\tif !slices.Contains(cfg.TrustedIssuers, req.Audience) {",
		"+\t\treturn fmt.Errorf(\"oauth: untrusted audience %q rejected by policy\", req.Audience)",
		"+\t}",
		"+\thasher := sha256.New()",
		"+\thasher.Write([]byte(req.ClientID + \":\" + strings.Join(req.AllowedScopes, \",\")))",
		"+\tsum := hex.EncodeToString(hasher.Sum(nil))",
		"+\tlog.Printf(\"validated token exchange digest=%s rotation=%ds\", sum[:12], req.RotationWindow)",
		"+\treturn cfg.AuditRecorder.RecordCheck(ctx, req.Audience, sum)",
		"+}",
		"+/* JSON schema excerpt for downstream verification:",
		"+   {\"$schema\":\"https://json-schema.org/draft/2020-12/schema\",\"title\":\"RefreshPolicy\",",
		"+    \"properties\":{\"kms_key_alias\":{\"type\":\"string\"},\"max_lease_seconds\":{\"type\":\"integer\"},",
		"+    \"strict_ip_binding\":{\"type\":\"boolean\"},\"fallback_jwks_uri\":{\"type\":\"string\"}}} */",
	}, "\n")
	if len(structured) <= 1200 {
		t.Fatalf("test fixture must exceed 1200 chars, got %d", len(structured))
	}
	structRho := ZlibDensityPenalty(structured)
	if structRho < 0.65 {
		t.Fatalf("expected structured Go diff density >= 0.65 (no false kill), got %.4f", structRho)
	}

	// Repetitive CI/test log spam (> 1200 chars) must be heavily penalized.
	spam := strings.Repeat("INFO [worker] heartbeat ok status=200 retry=0 queue_depth=0\n", 40)
	spamRho := ZlibDensityPenalty(spam)
	if spamRho >= 0.25 {
		t.Fatalf("expected repetitive log spam density < 0.25, got %.4f", spamRho)
	}
	if structRho <= spamRho*2.5 {
		t.Fatalf("expected structured diff (%.4f) strongly above repetitive spam (%.4f)", structRho, spamRho)
	}
}

func TestIndex_AdaptiveWeightsWhenNoEntitiesMatch(t *testing.T) {
	idx := NewIndex(DefaultConfig())
	var entries []session.LinearEntry
	// Early entry answers pure prose question (zero code entities).
	entries = append(entries, session.LinearEntry{
		EntryID: "e_early",
		Message: ai.NewAssistantMessage("线上发布的灰度放量策略是先开 5% 内部测试白名单观察半小时，再扩展到 30%。", ""),
	})
	// Many recent casual chat turns to test whether recency graph diffusion swamps BM25+Dense when entities=0.
	for i := 0; i < 12; i++ {
		entries = append(entries, session.LinearEntry{
			EntryID: fmt.Sprintf("e_chat_%02d", i),
			Message: ai.NewUserMessage(fmt.Sprintf("好的收到，我先看看第 %d 步输出再说。", i)),
		})
	}
	idx.Sync(entries)

	res := idx.SearchTopK("灰度放量策略白名单百分比是多少", 3, nil)
	if res.MatchedEntityCount != 0 {
		t.Fatalf("expected 0 graph matched entities for pure Chinese prose query, got %d", res.MatchedEntityCount)
	}
	if len(res.Hits) == 0 || res.Hits[0].Entry.EntryID != "e_early" {
		t.Fatalf("expected BM25+Dense dominance to rank e_early #1 when MatchedEntityCount=0, got %+v", res.Hits)
	}
}

func TestACTRTracker_SpacingEffect(t *testing.T) {
	actr := NewACTRTracker(0.5, -1.2, 1.0)
	actr.EnsureTurn(0, 1) // created at turn 1, reinforced at turns 10 and 25
	actr.EnsureTurn(1, 1) // created at turn 1, never reinforced

	actr.RecordAccess(0, 10)
	actr.RecordAccess(0, 25)
	// Duplicate access on same turn is deduplicated.
	actr.RecordAccess(0, 25)

	wReinforced := actr.TimeWeight(0, 30)
	wStale := actr.TimeWeight(1, 30)
	if wReinforced <= wStale {
		t.Fatalf("expected ACT-R reinforced turn (%.4f) > unreinforced turn (%.4f)", wReinforced, wStale)
	}
	if len(actr.accesses[0]) != 3 {
		t.Fatalf("expected 3 deduplicated accesses, got %d", len(actr.accesses[0]))
	}
}

func TestSelectBudgetedMMR_CostFloor256AndDeduplication(t *testing.T) {
	emb := NewHashNGramEmbedder(128)
	vCodeA := emb.Embed("internal/agent/compact.go ApplyEpochObservationMasking freezeBoundary")
	vCodeB := emb.Embed("internal/core/storage/sqlite.go ensureContextMetricsColumns migration")

	cands := []Candidate{
		// Short 16-token chatter with lower relevance weight (0.45): costFloor=256 prevents c^-0.5 exploitation.
		{TurnIdx: 0, EntryID: "short_chat", Weight: 0.45, Tokens: 16, Vec: emb.Embed("ok got it thanks")},
		// Medium 256-token code turn A with higher weight (0.85): must beat short_chat cleanly!
		{TurnIdx: 1, EntryID: "code_a", Weight: 0.85, Tokens: 256, Vec: vCodeA, Entities: map[string]struct{}{"compact.go": {}}},
		// Near-duplicate of code_a with weight 0.84.
		{TurnIdx: 2, EntryID: "code_a_dup", Weight: 0.84, Tokens: 256, Vec: vCodeA, Entities: map[string]struct{}{"compact.go": {}}},
		// Distinct code turn B with slightly lower weight (0.72): MMR lambda=0.70 should pick code_b before code_a_dup!
		{TurnIdx: 3, EntryID: "code_b", Weight: 0.72, Tokens: 256, Vec: vCodeB, Entities: map[string]struct{}{"sqlite.go": {}}},
	}

	chosen := SelectBudgetedMMR(cands, 512, 0.70, 0.50)
	if len(chosen) != 2 {
		t.Fatalf("expected 2 chosen items within 512 budget, got %d (%+v)", len(chosen), chosen)
	}
	if chosen[0].EntryID != "code_a" {
		t.Fatalf("expected code_a selected first under 256 cost floor, got %s", chosen[0].EntryID)
	}
	if chosen[1].EntryID != "code_b" {
		t.Fatalf("expected diverse code_b selected second ahead of duplicate/chatter, got %s", chosen[1].EntryID)
	}
}

func TestIndex_IncrementalSyncAndBranchSwitchRebuild(t *testing.T) {
	idx := NewIndex(DefaultConfig())
	e1 := session.LinearEntry{EntryID: "e1", Message: ai.NewUserMessage("check internal/agent/loop.go")}
	e2 := session.LinearEntry{EntryID: "e2", Message: ai.NewAssistantMessage("checked loop.go", "")}
	e3 := session.LinearEntry{EntryID: "e3", Message: ai.NewUserMessage("now inspect sqlite.go")}

	if rebuilt := idx.Sync([]session.LinearEntry{e1, e2}); !rebuilt || idx.Len() != 2 {
		t.Fatalf("expected initial build len=2, got rebuilt=%v len=%d", rebuilt, idx.Len())
	}
	if rebuilt := idx.Sync([]session.LinearEntry{e1, e2, e3}); rebuilt || idx.Len() != 3 {
		t.Fatalf("expected incremental append without rebuild, got rebuilt=%v len=%d", rebuilt, idx.Len())
	}
	// Simulate branch switch altering e2 -> e2_alt
	e2Alt := session.LinearEntry{EntryID: "e2_alt", Message: ai.NewAssistantMessage("alternative branch", "")}
	if rebuilt := idx.Sync([]session.LinearEntry{e1, e2Alt}); !rebuilt || idx.Len() != 2 {
		t.Fatalf("expected branch divergence to rebuild cleanly, got rebuilt=%v len=%d", rebuilt, idx.Len())
	}
}

func TestStripUserPromptBoilerplate_CleansWorkingSetAndPreservesPlainMessages(t *testing.T) {
	personaHeader := "[角色人设]\n你是一个严谨的资深研发助手，请先给结论再展开细节，保持简洁克制。\n" +
		strings.Repeat("回答风格约束：禁止编造文件与测试结果。\n", 6) +
		"[飞书上下文: chat_id=oc_abc123 | 发送者: ou_xyz | 重要说明: 生成图片、文件后无需贴本地路径，网关会自动上传并展示到飞书卡片]"

	wrapped := personaHeader + "\n请排查 internal/agent/compact.go 中 BuildWorkingSet 逻辑"
	if got := StripUserPromptBoilerplate(wrapped); got != "请排查 internal/agent/compact.go 中 BuildWorkingSet 逻辑" {
		t.Fatalf("unexpected stripped full wrapper: %q", got)
	}

	ctxOnly := "[飞书上下文: chat_id=oc_999 | 发送者: ou_111]\n查看 sqlite.go 迁移逻辑"
	if got := StripUserPromptBoilerplate(ctxOnly); got != "查看 sqlite.go 迁移逻辑" {
		t.Fatalf("unexpected stripped ctx-only wrapper: %q", got)
	}

	if got := StripUserPromptBoilerplate(personaHeader); got != "" {
		t.Fatalf("expected empty string when message contains only wrapper, got %q", got)
	}

	plain := "plain CLI prompt without gateway wrapper: AuditSlotVerifier"
	if got := StripUserPromptBoilerplate(plain); got != plain {
		t.Fatalf("expected plain user message preserved unchanged, got %q", got)
	}

	// Build an index where 6 RoleUser messages share the huge Feishu wrapper alongside plain RoleUser & code RoleTool entries.
	idx := NewIndex(DefaultConfig())
	var entries []session.LinearEntry
	for i := 0; i < 6; i++ {
		entries = append(entries, session.LinearEntry{
			EntryID: fmt.Sprintf("u_feishu_%d", i),
			Message: ai.NewUserMessage(fmt.Sprintf("%s\n日常确认第 %d 步进度，继续处理。", personaHeader, i)),
		})
	}
	// Empty-only wrapped user message (must be skipped by indexing/candidate scoring)
	entries = append(entries, session.LinearEntry{
		EntryID: "u_empty_wrapper",
		Message: ai.NewUserMessage(personaHeader + "\n   "),
	})
	// Real technical user message (without wrapper) & RoleTool output
	entries = append(entries,
		session.LinearEntry{
			EntryID: "u_plain_tech",
			Message: ai.NewUserMessage("Notice BuildWorkingSet calls SelectWorkingSet on entity TokenRotationPolicy"),
		},
		session.LinearEntry{
			EntryID: "t_code_output",
			Message: ai.Message{
				Role:       ai.RoleTool,
				ToolCallID: "call_tech_1",
				Content:    "internal/agent/compact.go: func BuildWorkingSet handles TokenRotationPolicy cleanly without duplicating persona boilerplate.",
			},
		},
	)
	idx.Sync(entries)

	// Query itself carries the Feishu wrapper (as seen in production latestUserQuery if raw, or cleaned).
	res := idx.SelectWorkingSet(StripUserPromptBoilerplate(personaHeader+"\n分析 TokenRotationPolicy 与 compact.go 的关联"), 1200, nil)
	if len(res.Hits) == 0 {
		t.Fatalf("expected WorkingSet hits, got 0")
	}
	var sawTool, sawPlain bool
	for _, h := range res.Hits {
		if h.Entry.EntryID == "u_empty_wrapper" {
			t.Fatalf("empty-after-strip RoleUser must never enter WorkingSet")
		}
		if strings.Contains(h.Rendered, "[角色人设]") || strings.Contains(h.Rendered, "[飞书上下文:") {
			t.Fatalf("gateway boilerplate leaked into WorkingSet rendered entry %s: %q", h.Entry.EntryID, h.Rendered)
		}
		if h.Entry.EntryID == "t_code_output" {
			sawTool = true
		}
		if h.Entry.EntryID == "u_plain_tech" {
			sawPlain = true
		}
	}
	if !sawTool || !sawPlain {
		t.Fatalf("expected code RoleTool (%v) and plain RoleUser (%v) recalled cleanly without boilerplate interference; hits=%+v", sawTool, sawPlain, res.Hits)
	}
}
