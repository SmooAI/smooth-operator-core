// Package core is the native Go smooth-operator engine — an in-process agentic
// tool-calling loop over an OpenAI-compatible chat client, with in-memory
// knowledge grounding. The Go sibling of the Rust reference engine, the C# core,
// the Python core, and the TypeScript core. See docs/Architecture/Go Core.md.
package core

import (
	"regexp"
	"sort"
	"strings"
)

var tokenRe = regexp.MustCompile(`[a-z0-9]+`)

func tokenize(text string) []string {
	return tokenRe.FindAllString(strings.ToLower(text), -1)
}

// KnowledgeHit is one retrieved document with its lexical-overlap score.
type KnowledgeHit struct {
	Content string
	Source  string
	Score   float64
}

// Knowledge is a retriever: returns the most relevant documents for a query. Both
// the lexical InMemoryKnowledge and the embedding-backed VectorKnowledge satisfy
// this, so the agent accepts either.
type Knowledge interface {
	Query(query string, topK int) []KnowledgeHit
}

type doc struct {
	content string
	source  string
}

// InMemoryKnowledge is a tiny lexical-overlap knowledge base — Phase-0 parity
// with the reference engines' in-memory lexical store (not a vector store).
type InMemoryKnowledge struct {
	docs []doc
}

// Ingest adds a document to the knowledge base.
func (k *InMemoryKnowledge) Ingest(content, source string) {
	k.docs = append(k.docs, doc{content: content, source: source})
}

// Query returns up to topK documents, ranked by token overlap with the query.
// When nothing overlaps, the first topK documents are returned anyway so the
// agent still has context to ground (or honestly decline) against.
func (k *InMemoryKnowledge) Query(query string, topK int) []KnowledgeHit {
	if topK <= 0 {
		topK = 4
	}
	qTokens := map[string]struct{}{}
	for _, t := range tokenize(query) {
		qTokens[t] = struct{}{}
	}

	type scored struct {
		overlap int
		d       doc
	}
	all := make([]scored, 0, len(k.docs))
	for _, d := range k.docs {
		overlap := 0
		seen := map[string]struct{}{}
		for _, t := range tokenize(d.content) {
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			if _, ok := qTokens[t]; ok {
				overlap++
			}
		}
		all = append(all, scored{overlap: overlap, d: d})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].overlap > all[j].overlap })

	hits := make([]KnowledgeHit, 0, topK)
	for _, s := range all {
		if len(hits) >= topK || s.overlap == 0 {
			break
		}
		hits = append(hits, KnowledgeHit{Content: s.d.content, Source: s.d.source, Score: float64(s.overlap)})
	}
	if len(hits) == 0 {
		for i, d := range k.docs {
			if i >= topK {
				break
			}
			hits = append(hits, KnowledgeHit{Content: d.content, Source: d.source, Score: 0})
		}
	}
	return hits
}
