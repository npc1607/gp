package retrieval

import (
	"math"
	"sort"
	"strings"
	"time"

	"gp/internal/ai"
	"gp/internal/core/compaction"
	"gp/internal/core/session"
)

// Config holds calibrated hyperparameters for hybrid PPR + ACT-R + MMR retrieval.
type Config struct {
	EmbedDim         int
	Alpha            float64
	PPRIters         int
	DegreeBeta       float64
	Lambda           float64
	Eta              float64
	ACTRDecay        float64
	ACTRTauB         float64
	ACTRS            float64
	RRFK             float64
	LogScale         float64
	RecentSeedWindow int
}

// DefaultConfig returns the tuned defaults from gp-context-lab ablation round 2.
func DefaultConfig() Config {
	return Config{
		EmbedDim:         128,
		Alpha:            0.75,
		PPRIters:         20,
		DegreeBeta:       0.4,
		Lambda:           0.70,
		Eta:              0.50,
		ACTRDecay:        0.5,
		ACTRTauB:         -1.2,
		ACTRS:            1.0,
		RRFK:             15.0,
		LogScale:         120.0,
		RecentSeedWindow: 5,
	}
}

// IndexedEntry holds precomputed lexical, dense, entity, and density features for one LinearEntry.
type IndexedEntry struct {
	Entry    session.LinearEntry
	Rendered string
	Tokens   int
	Vec      []float32
	Entities []string
	EntSet   map[string]struct{}
	Density  float64
}

// SearchResult captures a ranked or selected entry with its score, entities, and rendered payload.
type SearchResult struct {
	Entry           session.LinearEntry
	TurnIdx         int
	Score           float64
	Tokens          int
	Rendered        string
	Entities        []string
	MatchedEntities []string
}

// QueryResult summarizes a retrieval run along with latency and matched graph entity telemetry.
type QueryResult struct {
	Hits               []SearchResult
	MatchedEntityCount int
	BuildMs            float64
}

// Index maintains incremental BM25, EntityGraph, dense vectors, and ACT-R state over branch entries.
type Index struct {
	cfg        Config
	embedder   Embedder
	extractor  EntityExtractor
	bm25       *BM25
	graph      *EntityGraph
	actr       *ACTRTracker
	entries    []IndexedEntry
	entityHist map[string][]int
}

// NewIndex initializes an incremental retrieval index with calibrated defaults.
func NewIndex(cfg Config) *Index {
	def := DefaultConfig()
	if cfg.EmbedDim <= 0 {
		cfg.EmbedDim = def.EmbedDim
	}
	if cfg.Alpha <= 0 || cfg.Alpha >= 1 {
		cfg.Alpha = def.Alpha
	}
	if cfg.PPRIters <= 0 {
		cfg.PPRIters = def.PPRIters
	}
	if cfg.DegreeBeta < 0 {
		cfg.DegreeBeta = def.DegreeBeta
	}
	if cfg.Lambda <= 0 || cfg.Lambda >= 1 {
		cfg.Lambda = def.Lambda
	}
	if cfg.Eta < 0 {
		cfg.Eta = def.Eta
	}
	if cfg.ACTRDecay <= 0 {
		cfg.ACTRDecay = def.ACTRDecay
	}
	if cfg.ACTRS <= 0 {
		cfg.ACTRS = def.ACTRS
	}
	if cfg.ACTRTauB == 0 {
		cfg.ACTRTauB = def.ACTRTauB
	}
	if cfg.RRFK <= 0 {
		cfg.RRFK = def.RRFK
	}
	if cfg.LogScale <= 0 {
		cfg.LogScale = def.LogScale
	}
	if cfg.RecentSeedWindow <= 0 {
		cfg.RecentSeedWindow = def.RecentSeedWindow
	}
	idx := &Index{
		cfg:       cfg,
		embedder:  NewHashNGramEmbedder(cfg.EmbedDim),
		extractor: HeuristicExtractor{},
	}
	idx.reset()
	return idx
}

func (idx *Index) reset() {
	idx.bm25 = NewBM25()
	idx.graph = NewEntityGraph()
	idx.actr = NewACTRTracker(idx.cfg.ACTRDecay, idx.cfg.ACTRTauB, idx.cfg.ACTRS)
	idx.entries = nil
	idx.entityHist = make(map[string][]int)
}

// Len returns the number of entries currently indexed.
func (idx *Index) Len() int {
	return len(idx.entries)
}

// Sync incrementally indexes new branch entries when the existing prefix matches, or rebuilds if diverged.
func (idx *Index) Sync(branchEntries []session.LinearEntry) (rebuilt bool) {
	if idx.bm25 == nil || len(idx.entries) == 0 || len(branchEntries) < len(idx.entries) {
		idx.reset()
		rebuilt = len(branchEntries) > 0
	} else {
		for i := range idx.entries {
			if idx.entries[i].Entry.EntryID != branchEntries[i].EntryID ||
				idx.entries[i].Entry.Message.Content != branchEntries[i].Message.Content {
				idx.reset()
				rebuilt = true
				break
			}
		}
	}
	for i := len(idx.entries); i < len(branchEntries); i++ {
		idx.ingestOne(branchEntries[i])
	}
	return rebuilt
}

func (idx *Index) ingestOne(le session.LinearEntry) {
	entryIdx := len(idx.entries)
	rendered := RenderMessageText(le.Message)
	if strings.TrimSpace(rendered) == "" {
		// Preserve 1:1 alignment with branchEntries for incremental Sync without indexing empty entries.
		idx.entries = append(idx.entries, IndexedEntry{Entry: le})
		idx.bm25.AddDoc("")
		idx.graph.AddMessage(entryIdx, le.Message, nil, nil)
		idx.actr.EnsureTurn(entryIdx, entryIdx)
		return
	}

	toks := int(compaction.EstimateTextTokens(rendered))
	if toks < 1 {
		toks = 1
	}
	vec := idx.embedder.Embed(rendered)
	ents := idx.extractor.ExtractMessage(le.Message)
	entSet := make(map[string]struct{}, len(ents))
	for _, e := range ents {
		entSet[e] = struct{}{}
	}

	idx.entries = append(idx.entries, IndexedEntry{
		Entry:    le,
		Rendered: rendered,
		Tokens:   toks,
		Vec:      vec,
		Entities: ents,
		EntSet:   entSet,
		Density:  ZlibDensityPenalty(rendered),
	})

	idx.bm25.AddDoc(rendered)
	idx.graph.AddMessage(entryIdx, le.Message, ents, vec)
	idx.actr.EnsureTurn(entryIdx, entryIdx)
	for _, e := range ents {
		for _, prevIdx := range idx.entityHist[e] {
			idx.actr.RecordAccess(prevIdx, entryIdx)
		}
		idx.entityHist[e] = append(idx.entityHist[e], entryIdx)
	}
}

// SelectWorkingSet scores eligible non-verbatim entries and chooses a diverse token-budgeted subset
// returned in chronological TurnIdx order.
func (idx *Index) SelectWorkingSet(query string, tokenBudget int, excludeEntryIDs map[string]struct{}) QueryResult {
	start := time.Now()
	cands, matchedQueryEnts := idx.scoreCandidates(query, excludeEntryIDs)
	if len(cands) == 0 || tokenBudget <= 0 {
		return QueryResult{
			MatchedEntityCount: len(matchedQueryEnts),
			BuildMs:            float64(time.Since(start).Microseconds()) / 1000.0,
		}
	}

	chosen := SelectBudgetedMMR(cands, tokenBudget, idx.cfg.Lambda, idx.cfg.Eta)
	sort.Slice(chosen, func(i, j int) bool {
		return chosen[i].TurnIdx < chosen[j].TurnIdx
	})
	return QueryResult{
		Hits:               idx.toSearchResults(chosen, matchedQueryEnts),
		MatchedEntityCount: len(matchedQueryEnts),
		BuildMs:            float64(time.Since(start).Microseconds()) / 1000.0,
	}
}

// SearchTopK returns up to topK diversity-aware hits across indexed entries ordered by MMR selection rank.
func (idx *Index) SearchTopK(query string, topK int, excludeEntryIDs map[string]struct{}) QueryResult {
	start := time.Now()
	if topK <= 0 {
		topK = 6
	}
	cands, matchedQueryEnts := idx.scoreCandidates(query, excludeEntryIDs)
	if len(cands) == 0 {
		return QueryResult{
			MatchedEntityCount: len(matchedQueryEnts),
			BuildMs:            float64(time.Since(start).Microseconds()) / 1000.0,
		}
	}

	// Normalize length cost uniformly when choosing top-K search previews so multi-KB tool outputs
	// are not penalized below short lines while still keeping MMR pairwise deduplication.
	uniformCands := make([]Candidate, len(cands))
	copy(uniformCands, cands)
	for i := range uniformCands {
		uniformCands[i].Tokens = int(mmrCostFloor)
	}
	chosen := SelectBudgetedMMR(uniformCands, topK*int(mmrCostFloor), idx.cfg.Lambda, 0)
	return QueryResult{
		Hits:               idx.toSearchResults(chosen, matchedQueryEnts),
		MatchedEntityCount: len(matchedQueryEnts),
		BuildMs:            float64(time.Since(start).Microseconds()) / 1000.0,
	}
}

func (idx *Index) toSearchResults(cands []Candidate, queryEnts []string) []SearchResult {
	qSet := make(map[string]struct{}, len(queryEnts))
	for _, e := range queryEnts {
		qSet[e] = struct{}{}
	}
	out := make([]SearchResult, 0, len(cands))
	for _, c := range cands {
		ie := idx.entries[c.TurnIdx]
		var matched []string
		for _, e := range ie.Entities {
			if _, ok := qSet[e]; ok {
				matched = append(matched, e)
			}
		}
		out = append(out, SearchResult{
			Entry:           ie.Entry,
			TurnIdx:         c.TurnIdx,
			Score:           c.Weight,
			Tokens:          ie.Tokens,
			Rendered:        ie.Rendered,
			Entities:        ie.Entities,
			MatchedEntities: matched,
		})
	}
	return out
}

func (idx *Index) scoreCandidates(query string, excludeEntryIDs map[string]struct{}) ([]Candidate, []string) {
	n := len(idx.entries)
	if n == 0 || strings.TrimSpace(query) == "" {
		return nil, nil
	}

	qVec := idx.embedder.Embed(query)
	denseRaw := make([]float64, n)
	for i := 0; i < n; i++ {
		denseRaw[i] = math.Max(0, Cosine(qVec, idx.entries[i].Vec))
	}
	denseNorm := NormalizeMinMax(denseRaw)
	bm25Norm := NormalizeMinMax(idx.bm25.Score(query))

	qEnts := idx.extractor.ExtractQuery(query)
	var matchedQueryEnts []string
	seenMatched := make(map[string]struct{})
	for _, e := range qEnts {
		if _, ok := idx.graph.EntToNode[e]; ok {
			if _, dup := seenMatched[e]; !dup {
				seenMatched[e] = struct{}{}
				matchedQueryEnts = append(matchedQueryEnts, e)
			}
		}
	}
	hasQueryEntities := idx.graph.CountMatchedEntities(qEnts) > 0

	seedStart := n - idx.cfg.RecentSeedWindow
	if seedStart < 0 {
		seedStart = 0
	}
	recentIdxs := make([]int, 0, n-seedStart)
	for i := n - 1; i >= seedStart; i-- {
		recentIdxs = append(recentIdxs, i)
	}
	seed := idx.graph.BuildSeedDistribution(qEnts, recentIdxs)
	diffused := PersonalizedPageRank(idx.graph, seed, idx.cfg.Alpha, idx.cfg.PPRIters, idx.cfg.DegreeBeta)
	graphNorm := NormalizeMinMax(idx.graph.ProjectMessageScores(diffused, idx.cfg.LogScale))

	var rrfWeights []float64
	if hasQueryEntities {
		rrfWeights = []float64{0.25, 0.35, 0.55}
	} else {
		rrfWeights = []float64{0.25, 0.60, 0.15}
	}
	rrfScores := WeightedRRF([][]float64{denseNorm, bm25Norm, graphNorm}, rrfWeights, idx.cfg.RRFK)

	nowTurn := n - 1
	cands := make([]Candidate, 0, n)
	for i := 0; i < n; i++ {
		ie := idx.entries[i]
		if ie.Entry.Message.Role == ai.RoleSystem || strings.TrimSpace(ie.Rendered) == "" || ie.Tokens <= 0 {
			continue
		}
		if _, excluded := excludeEntryIDs[ie.Entry.EntryID]; excluded {
			continue
		}

		var baseRel float64
		if hasQueryEntities {
			baseRel = 0.20*denseRaw[i] + 0.35*bm25Norm[i] + 0.45*graphNorm[i]
		} else {
			baseRel = 0.25*denseRaw[i] + 0.65*bm25Norm[i] + 0.10*graphNorm[i]
		}
		if len(rrfScores) == n {
			baseRel = 0.55*baseRel + 0.45*rrfScores[i]
		}
		timeW := idx.actr.TimeWeight(i, nowTurn)
		w := (0.3 + 0.7*timeW) * ie.Density * baseRel
		if w <= 0 {
			continue
		}

		cands = append(cands, Candidate{
			TurnIdx:  i,
			EntryID:  ie.Entry.EntryID,
			Weight:   w,
			Tokens:   ie.Tokens,
			Vec:      ie.Vec,
			Entities: ie.EntSet,
		})
	}

	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Weight == cands[j].Weight {
			return cands[i].TurnIdx > cands[j].TurnIdx
		}
		return cands[i].Weight > cands[j].Weight
	})
	if len(cands) > 200 {
		cands = cands[:200]
	}
	if len(cands) > 0 && cands[0].Weight > 0 {
		maxW := cands[0].Weight
		for i := range cands {
			cands[i].Weight /= maxW
		}
	}
	return cands, matchedQueryEnts
}
