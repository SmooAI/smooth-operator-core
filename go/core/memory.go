package core

import (
	"sort"
	"strings"
)

// Long-term memory — facts the agent carries across conversations.
//
// Phase-1 sibling of the reference engines' memory. Distinct from checkpointing
// (which persists a single conversation's messages): Memory is a durable pool of
// standalone facts the agent recalls into context on any turn, keyed by relevance
// to the current message. InMemoryMemory is the zero-dependency default (lexical
// recall); a vector-backed memory drops in behind the interface.

// MemoryEntry is one remembered fact.
type MemoryEntry struct {
	Text string
}

// Memory is a pool of remembered facts, recalled by relevance to a query.
type Memory interface {
	Remember(text string)
	Recall(query string, topK int) []MemoryEntry
}

// InMemoryMemory is a process-local memory pool with lexical-overlap recall.
type InMemoryMemory struct {
	entries []MemoryEntry
}

// Remember adds a fact (blank entries are ignored).
func (m *InMemoryMemory) Remember(text string) {
	text = strings.TrimSpace(text)
	if text != "" {
		m.entries = append(m.entries, MemoryEntry{Text: text})
	}
}

// Recall returns up to topK entries that share terms with the query, best first.
func (m *InMemoryMemory) Recall(query string, topK int) []MemoryEntry {
	if topK <= 0 {
		return nil
	}
	qTerms := map[string]struct{}{}
	for _, t := range tokenize(query) {
		qTerms[t] = struct{}{}
	}

	type scored struct {
		overlap int
		entry   MemoryEntry
	}
	var matched []scored
	for _, e := range m.entries {
		overlap := 0
		seen := map[string]struct{}{}
		for _, t := range tokenize(e.Text) {
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			if _, ok := qTerms[t]; ok {
				overlap++
			}
		}
		if overlap > 0 {
			matched = append(matched, scored{overlap: overlap, entry: e})
		}
	}
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].overlap > matched[j].overlap })

	out := make([]MemoryEntry, 0, topK)
	for i, s := range matched {
		if i >= topK {
			break
		}
		out = append(out, s.entry)
	}
	return out
}
