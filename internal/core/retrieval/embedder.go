package retrieval

import (
	"hash/fnv"
	"math"
	"strings"
)

// Embedder maps text into a unit-normalized dense vector.
type Embedder interface {
	Dim() int
	Embed(text string) []float32
}

// HashNGramEmbedder provides zero-dependency deterministic local embeddings via signed random
// projection of code tokens and character 3-grams.
type HashNGramEmbedder struct {
	dim int
}

// NewHashNGramEmbedder creates a local deterministic embedder of dimension dim (default 128).
func NewHashNGramEmbedder(dim int) *HashNGramEmbedder {
	if dim <= 0 {
		dim = 128
	}
	return &HashNGramEmbedder{dim: dim}
}

// Dim returns the output vector dimension.
func (e *HashNGramEmbedder) Dim() int { return e.dim }

// Embed projects text into a unit-length float32 vector.
func (e *HashNGramEmbedder) Embed(text string) []float32 {
	vec := make([]float32, e.dim)
	if text == "" {
		return vec
	}
	for _, tok := range CodeTokenize(text) {
		addFeature(vec, "t:"+tok, 1.5)
	}
	lower := strings.ToLower(text)
	runes := []rune(lower)
	for i := 0; i+3 <= len(runes); i++ {
		addFeature(vec, "g:"+string(runes[i:i+3]), 0.35)
	}
	l2Normalize(vec)
	return vec
}

// Cosine computes dot product / cosine similarity between two unit-normalized vectors.
func Cosine(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var sum float64
	for i := 0; i < n; i++ {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

func addFeature(vec []float32, feat string, weight float32) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(feat))
	v := h.Sum64()
	idx := int(v % uint64(len(vec)))
	sign := float32(1.0)
	if (v>>63)&1 == 1 {
		sign = -1.0
	}
	vec[idx] += sign * weight
}

func l2Normalize(vec []float32) {
	var sq float64
	for _, v := range vec {
		sq += float64(v) * float64(v)
	}
	if sq == 0 {
		return
	}
	inv := float32(1.0 / math.Sqrt(sq))
	for i := range vec {
		vec[i] *= inv
	}
}
