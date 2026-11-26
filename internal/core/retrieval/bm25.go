package retrieval

import "math"

// BM25 maintains an incremental sparse inverted index over documents.
type BM25 struct {
	k1       float64
	b        float64
	docLens  []int
	avgLen   float64
	totalLn  int
	postings map[string][]posting
}

type posting struct {
	docID int
	tf    int
}

// NewBM25 constructs an incremental BM25 index with standard parameters (k1=1.5, b=0.75).
func NewBM25() *BM25 {
	return &BM25{
		k1:       1.5,
		b:        0.75,
		postings: make(map[string][]posting),
	}
}

// AddDoc indexes a document and returns its assigned zero-based docID.
func (idx *BM25) AddDoc(text string) int {
	terms := CodeTokenize(text)
	docID := len(idx.docLens)
	idx.docLens = append(idx.docLens, len(terms))
	idx.totalLn += len(terms)
	idx.avgLen = float64(idx.totalLn) / float64(len(idx.docLens))

	tf := make(map[string]int, len(terms))
	for _, t := range terms {
		tf[t]++
	}
	for t, count := range tf {
		idx.postings[t] = append(idx.postings[t], posting{docID: docID, tf: count})
	}
	return docID
}

// Score computes BM25 relevance scores across all indexed documents for query.
func (idx *BM25) Score(query string) []float64 {
	n := len(idx.docLens)
	scores := make([]float64, n)
	if n == 0 || idx.avgLen == 0 {
		return scores
	}
	qTerms := CodeTokenize(query)
	seen := make(map[string]struct{}, len(qTerms))
	for _, qt := range qTerms {
		if _, ok := seen[qt]; ok {
			continue
		}
		seen[qt] = struct{}{}
		plist := idx.postings[qt]
		df := len(plist)
		if df == 0 {
			continue
		}
		idf := math.Log(1.0 + (float64(n)-float64(df)+0.5)/(float64(df)+0.5))
		for _, p := range plist {
			dl := float64(idx.docLens[p.docID])
			tf := float64(p.tf)
			denom := tf + idx.k1*(1.0-idx.b+idx.b*(dl/idx.avgLen))
			scores[p.docID] += idf * (tf * (idx.k1 + 1.0)) / denom
		}
	}
	return scores
}
