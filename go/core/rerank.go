package core

import (
	"math"
	"sort"
)

// Knowledge reranking — reorder retrieved documents by relevance.
//
// Phase-1 sibling of the reference engines' reranking. A Reranker takes the query
// and the retriever's candidate hits and returns them reordered (and possibly
// trimmed). NoopReranker is the default passthrough; LexicalReranker re-scores by
// query-term coverage normalized for document length, so a concise on-topic doc
// outranks a long one with the same raw overlap. A cross-encoder or gateway
// reranker drops in behind the same interface.

// Reranker reorders retrieved hits by relevance to the query.
type Reranker interface {
	Rerank(query string, hits []KnowledgeHit) []KnowledgeHit
}

// NoopReranker returns the hits unchanged — the zero-cost default.
type NoopReranker struct{}

// Rerank returns hits unchanged.
func (NoopReranker) Rerank(_ string, hits []KnowledgeHit) []KnowledgeHit { return hits }

// LexicalReranker reorders by query-term coverage normalized by document length:
// coverage / log2(2 + docTokenCount), so coverage is rewarded but long documents
// are penalized relative to concise ones with the same coverage. Stable for ties.
type LexicalReranker struct{}

// Rerank reorders hits by the normalized-coverage score.
func (LexicalReranker) Rerank(query string, hits []KnowledgeHit) []KnowledgeHit {
	qTerms := map[string]struct{}{}
	for _, t := range tokenize(query) {
		qTerms[t] = struct{}{}
	}
	if len(qTerms) == 0 {
		return hits
	}

	score := func(h KnowledgeHit) float64 {
		docTokens := tokenize(h.Content)
		coverage := 0
		seen := map[string]struct{}{}
		for _, t := range docTokens {
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			if _, ok := qTerms[t]; ok {
				coverage++
			}
		}
		return float64(coverage) / math.Log2(2+float64(len(docTokens)))
	}

	out := make([]KnowledgeHit, len(hits))
	copy(out, hits)
	sort.SliceStable(out, func(i, j int) bool { return score(out[i]) > score(out[j]) })
	return out
}
