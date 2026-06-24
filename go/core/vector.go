package core

import (
	"math"
	"sort"
)

// Vector knowledge — embedding-backed semantic retrieval.
//
// Phase-1 sibling of the reference engines' vector store. VectorKnowledge embeds
// documents and queries and retrieves by cosine similarity, satisfying the same
// Knowledge interface as the lexical retriever (so the agent accepts either). The
// Embedder is pluggable: the default HashEmbedder is deterministic and offline
// (feature-hashed bag-of-words) — good for tests and a zero-dependency default —
// while a gateway embedder drops in behind the same interface for true semantics.

// hashToken is a small deterministic non-cryptographic hash (FNV-1a, 32-bit).
func hashToken(token string) uint32 {
	var h uint32 = 0x811c9dc5
	for i := 0; i < len(token); i++ {
		h ^= uint32(token[i])
		h *= 0x01000193
	}
	return h
}

// Embedder turns text into a fixed-length vector.
type Embedder interface {
	Embed(text string) []float64
}

// HashEmbedder is a deterministic, offline feature-hashing embedder. It hashes each
// token into one of dim buckets (signed) and L2-normalizes. No learned semantics,
// but a real vector with cosine geometry — docs sharing tokens land near each other.
type HashEmbedder struct {
	Dim int
}

// NewHashEmbedder returns a HashEmbedder with the given dimension (default 256).
func NewHashEmbedder(dim int) HashEmbedder {
	if dim <= 0 {
		dim = 256
	}
	return HashEmbedder{Dim: dim}
}

// Embed hashes tokens into a normalized vector.
func (e HashEmbedder) Embed(text string) []float64 {
	dim := e.Dim
	if dim <= 0 {
		dim = 256
	}
	vec := make([]float64, dim)
	for _, tok := range tokenize(text) {
		h := hashToken(tok)
		bucket := int(h % uint32(dim))
		if (h>>31)&1 == 1 {
			vec[bucket]--
		} else {
			vec[bucket]++
		}
	}
	var norm float64
	for _, v := range vec {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for i := range vec {
			vec[i] /= norm
		}
	}
	return vec
}

func cosine(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i] // both L2-normalized
	}
	return s
}

type vecDoc struct {
	emb     []float64
	content string
	source  string
}

// VectorKnowledge is an embedding-backed knowledge store with cosine retrieval.
type VectorKnowledge struct {
	embedder Embedder
	docs     []vecDoc
}

// NewVectorKnowledge constructs a store with the given embedder (default HashEmbedder).
func NewVectorKnowledge(embedder Embedder) *VectorKnowledge {
	if embedder == nil {
		embedder = NewHashEmbedder(256)
	}
	return &VectorKnowledge{embedder: embedder}
}

// Ingest embeds and stores a document.
func (v *VectorKnowledge) Ingest(content, source string) {
	v.docs = append(v.docs, vecDoc{emb: v.embedder.Embed(content), content: content, source: source})
}

// Query embeds the query and returns up to topK docs by cosine similarity.
func (v *VectorKnowledge) Query(query string, topK int) []KnowledgeHit {
	if topK <= 0 || len(v.docs) == 0 {
		return nil
	}
	q := v.embedder.Embed(query)
	hits := make([]KnowledgeHit, 0, len(v.docs))
	for _, d := range v.docs {
		score := cosine(q, d.emb)
		if score > 0 {
			hits = append(hits, KnowledgeHit{Content: d.content, Source: d.source, Score: score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}
