package retrieval

import "math"

// ACTRTracker tracks logical turn access histories and computes power-law base-level activation.
// Formula: B_i(t_now) = ln( sum_{k=1}^m (delta_t_k + delta)^(-d) ), W_time = sigmoid((B_i - tau_B) / s)
type ACTRTracker struct {
	d        float64
	delta    float64
	tauB     float64
	s        float64
	accesses [][]int // indexed by entryIdx -> list of logical turns where item was created/reinforced
}

// NewACTRTracker creates an ACT-R power-law forgetting tracker (defaults: d=0.5, tauB=-1.2, s=1.0).
func NewACTRTracker(d, tauB, s float64) *ACTRTracker {
	if d <= 0 {
		d = 0.5
	}
	if s <= 0 {
		s = 1.0
	}
	return &ACTRTracker{
		d:     d,
		delta: 1.0,
		tauB:  tauB,
		s:     s,
	}
}

// EnsureTurn ensures the slice covers entryIdx and records its initial creation logicalTurn.
func (a *ACTRTracker) EnsureTurn(entryIdx, logicalTurn int) {
	for len(a.accesses) <= entryIdx {
		a.accesses = append(a.accesses, nil)
	}
	a.RecordAccess(entryIdx, logicalTurn)
}

// RecordAccess appends a reinforcement logicalTurn for entryIdx (deduplicating repeated same-turn triggers).
func (a *ACTRTracker) RecordAccess(entryIdx, logicalTurn int) {
	if entryIdx < 0 || entryIdx >= len(a.accesses) {
		return
	}
	hist := a.accesses[entryIdx]
	if len(hist) > 0 && hist[len(hist)-1] == logicalTurn {
		return
	}
	a.accesses[entryIdx] = append(hist, logicalTurn)
}

// TimeWeight returns W_time(entryIdx, nowLogicalTurn) in (0, 1).
func (a *ACTRTracker) TimeWeight(entryIdx, nowLogicalTurn int) float64 {
	if entryIdx < 0 || entryIdx >= len(a.accesses) {
		return 0.5
	}
	var sum float64
	for _, t := range a.accesses[entryIdx] {
		dt := float64(nowLogicalTurn - t)
		if dt < 0 {
			dt = 0
		}
		sum += math.Pow(dt+a.delta, -a.d)
	}
	if sum <= 0 {
		return 0.0
	}
	b := math.Log(sum)
	return 1.0 / (1.0 + math.Exp(-(b-a.tauB)/a.s))
}
