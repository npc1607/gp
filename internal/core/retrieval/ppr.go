package retrieval

import "math"

// PersonalizedPageRank runs power iteration on g with entity hub degree penalty beta.
// Formula: r^{(k+1)} = (1 - alpha) * q + alpha * P^T * r^{(k)}
func PersonalizedPageRank(g *EntityGraph, seed []float64, alpha float64, iters int, beta float64) []float64 {
	n := len(g.Kinds)
	if n == 0 || len(seed) != n {
		return nil
	}
	if alpha <= 0 || alpha >= 1 {
		alpha = 0.75
	}
	if iters <= 0 {
		iters = 20
	}
	if beta < 0 {
		beta = 0.4
	}

	outSum := make([]float64, n)
	for u := 0; u < n; u++ {
		var s float64
		for _, e := range g.Adj[u] {
			degV := float64(len(g.Adj[e.To]))
			pen := 1.0
			if g.Kinds[e.To] == NodeEntity && degV > 1 && beta > 0 {
				pen = math.Pow(degV, -beta)
			}
			s += e.Weight * pen
		}
		outSum[u] = s
	}

	rCurr := make([]float64, n)
	rNext := make([]float64, n)
	copy(rCurr, seed)

	for it := 0; it < iters; it++ {
		for i := 0; i < n; i++ {
			rNext[i] = (1.0 - alpha) * seed[i]
		}
		var dangling float64
		for u := 0; u < n; u++ {
			ru := rCurr[u]
			if ru == 0 {
				continue
			}
			s := outSum[u]
			if s == 0 {
				dangling += ru
				continue
			}
			invS := alpha * ru / s
			for _, e := range g.Adj[u] {
				degV := float64(len(g.Adj[e.To]))
				pen := 1.0
				if g.Kinds[e.To] == NodeEntity && degV > 1 && beta > 0 {
					pen = math.Pow(degV, -beta)
				}
				rNext[e.To] += invS * e.Weight * pen
			}
		}
		if dangling > 0 {
			for i := 0; i < n; i++ {
				rNext[i] += alpha * dangling * seed[i]
			}
		}
		rCurr, rNext = rNext, rCurr
	}
	return rCurr
}
