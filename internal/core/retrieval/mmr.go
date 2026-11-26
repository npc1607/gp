package retrieval

import "math"

const mmrCostFloor = 256.0

// Candidate represents a scored historical entry eligible for WorkingSet or search selection.
type Candidate struct {
	TurnIdx  int
	EntryID  string
	Weight   float64
	Tokens   int
	Vec      []float32
	Entities map[string]struct{}
}

// PairwiseSimilarity combines normalized vector cosine similarity and entity-set Jaccard overlap.
func PairwiseSimilarity(a, b Candidate) float64 {
	cos := 0.0
	if len(a.Vec) > 0 && len(b.Vec) > 0 {
		cos = math.Max(0, Cosine(a.Vec, b.Vec))
	}
	jacc := Jaccard(a.Entities, b.Entities)
	if len(a.Entities) == 0 && len(b.Entities) == 0 {
		return cos
	}
	return 0.6*cos + 0.4*jacc
}

// Jaccard computes the Jaccard overlap between two string sets.
func Jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	if len(a) > len(b) {
		a, b = b, a
	}
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// SelectBudgetedMMR greedily selects a diverse subset within token budget:
// argmax (lambda * W(d_i) - (1-lambda) * max_{d_j in S} Sim(d_i, d_j)) / max(256, c_i)^eta
func SelectBudgetedMMR(cands []Candidate, budget int, lambda, eta float64) []Candidate {
	if budget <= 0 || len(cands) == 0 {
		return nil
	}
	if lambda <= 0 || lambda >= 1 {
		lambda = 0.70
	}
	if eta < 0 {
		eta = 0.50
	}

	used := make([]bool, len(cands))
	maxSimToSel := make([]float64, len(cands))
	selected := make([]Candidate, 0, 16)
	rem := budget

	for {
		bestIdx := -1
		bestScore := -math.MaxFloat64
		for i, c := range cands {
			if used[i] || c.Tokens <= 0 || c.Tokens > rem {
				continue
			}
			costNorm := math.Pow(math.Max(mmrCostFloor, float64(c.Tokens)), eta)
			marginal := lambda*c.Weight - (1.0-lambda)*maxSimToSel[i]
			score := marginal / costNorm
			if score > bestScore {
				bestScore = score
				bestIdx = i
			}
		}
		if bestIdx < 0 {
			break
		}
		if len(selected) > 0 && bestScore <= 0 {
			break
		}
		chosen := cands[bestIdx]
		used[bestIdx] = true
		selected = append(selected, chosen)
		rem -= chosen.Tokens

		for i, c := range cands {
			if used[i] || c.Tokens > rem {
				continue
			}
			sim := PairwiseSimilarity(c, chosen)
			if sim > maxSimToSel[i] {
				maxSimToSel[i] = sim
			}
		}
	}
	return selected
}
