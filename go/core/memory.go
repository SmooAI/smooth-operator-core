package core

import (
	"fmt"
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
//
// Recall behaviour here is a CROSS-LANGUAGE CONTRACT, not a local choice: the Rust
// reference (rust/smooth-operator-core/src/memory.rs) and the C#, Python and
// TypeScript siblings all produce the same block for the same memories. Pearl
// th-ffaeae exists because they had drifted — three spellings of the header alone,
// plus different scores, entry formats and top-k defaults, which made a shared
// conformance scenario impossible to write.

// MemoryTopK is how many memories are auto-recalled per turn when the caller does
// not set one. Matches MEMORY_TOP_K in every sibling core.
const MemoryTopK = 5

// RecallHeader opens every auto-recall block, in every core.
const RecallHeader = "[Recalled memories]"

// RecallFreshnessNote is the verify-before-recommend note (rule D6), emitted only
// when at least one recalled entry is time-sensitive (see NeedsFreshnessCheck).
//
// A memory naming a function, file, or flag is a claim about the PAST, not a fact
// about now — without this line the model happily recommends a symbol that was
// deleted three releases ago.
const RecallFreshnessNote = "Note: 'the memory says X exists' is not the same as 'X exists now'. " +
	"Before recommending or acting on any function path, file, flag, or external " +
	"pointer named below, verify it's current by reading the file or grepping the " +
	"codebase. Project and Reference memories are time-sensitive; User and Feedback " +
	"are durable."

// MemoryType is what kind of fact a memory holds. The name is rendered into the
// recall block, so these spellings are part of the cross-language contract.
type MemoryType string

// The memory kinds, spelled as they appear in the rendered block.
const (
	MemoryShortTerm MemoryType = "ShortTerm"
	MemoryLongTerm  MemoryType = "LongTerm"
	MemoryEntity    MemoryType = "Entity"
	MemoryUser      MemoryType = "User"
	MemoryFeedback  MemoryType = "Feedback"
	MemoryProject   MemoryType = "Project"
	MemoryReference MemoryType = "Reference"
)

// NeedsFreshnessCheck reports whether this kind of memory can go stale. Project and
// Reference entries name things in a moving codebase or an external system; User and
// Feedback describe the person, which does not change when someone renames a file.
func (t MemoryType) NeedsFreshnessCheck() bool {
	return t == MemoryProject || t == MemoryReference
}

// MemoryEntry is one remembered fact. Type and Relevance are rendered into the
// recall block; a zero Type reads as LongTerm.
type MemoryEntry struct {
	Text      string
	Type      MemoryType
	Relevance float64
}

// typeOrDefault treats the zero value as LongTerm, so an entry built by a store
// that does not classify still renders.
func (e MemoryEntry) typeOrDefault() MemoryType {
	if e.Type == "" {
		return MemoryLongTerm
	}
	return e.Type
}

// Memory is a pool of remembered facts, recalled by relevance to a query.
type Memory interface {
	Remember(text string)
	Recall(query string, topK int) []MemoryEntry
}

// RelevanceScore is the fraction of the QUERY's distinct tokens that appear in
// content.
//
// Normalised to 0-1 deliberately: a raw overlap count is comparable neither between
// entries nor between languages, and it is rendered into the prompt. Punctuation is
// a token separator — the Rust reference used to split on whitespace alone, so a
// query ending "…my name?" never matched a memory containing "name" and the entry
// was silently not recalled.
func RelevanceScore(query, content string) float64 {
	queryTokens := map[string]struct{}{}
	for _, t := range tokenize(query) {
		queryTokens[t] = struct{}{}
	}
	if len(queryTokens) == 0 {
		return 0
	}
	contentTokens := map[string]struct{}{}
	for _, t := range tokenize(content) {
		contentTokens[t] = struct{}{}
	}
	matching := 0
	for t := range queryTokens {
		if _, ok := contentTokens[t]; ok {
			matching++
		}
	}
	return float64(matching) / float64(len(queryTokens))
}

// RenderRecallBlock renders recalled entries as the context block injected into a
// turn. Byte-identical to the Rust reference's render_recall_block. Empty string for
// no entries, so a caller injects nothing rather than a bare header telling the model
// it remembered nothing.
func RenderRecallBlock(entries []MemoryEntry) string {
	if len(entries) == 0 {
		return ""
	}
	lines := []string{RecallHeader}
	for _, e := range entries {
		if e.typeOrDefault().NeedsFreshnessCheck() {
			lines = append(lines, RecallFreshnessNote)
			break
		}
	}
	for _, e := range entries {
		lines = append(lines, fmt.Sprintf("- (%s, relevance=%.2f): %s", e.typeOrDefault(), e.Relevance, e.Text))
	}
	return strings.Join(lines, "\n") + "\n"
}

// InMemoryMemory is a process-local memory pool with lexical-overlap recall.
type InMemoryMemory struct {
	entries []MemoryEntry
}

// Remember adds a fact (blank entries are ignored).
func (m *InMemoryMemory) Remember(text string) {
	m.RememberTyped(text, MemoryLongTerm)
}

// RememberTyped adds a fact of a given kind. Separate from Remember rather than a
// second parameter so the Memory interface — which host code implements — stays
// source-compatible.
func (m *InMemoryMemory) RememberTyped(text string, memoryType MemoryType) {
	text = strings.TrimSpace(text)
	if text != "" {
		m.entries = append(m.entries, MemoryEntry{Text: text, Type: memoryType})
	}
}

// Recall returns up to topK entries that share tokens with the query, best first.
func (m *InMemoryMemory) Recall(query string, topK int) []MemoryEntry {
	if topK <= 0 {
		return nil
	}
	var matched []MemoryEntry
	for _, e := range m.entries {
		if score := RelevanceScore(query, e.Text); score > 0 {
			hit := e
			hit.Relevance = score
			matched = append(matched, hit)
		}
	}
	// Best first; stable, so ties keep insertion order — the sibling cores rely on
	// that to produce the same block.
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].Relevance > matched[j].Relevance })

	if len(matched) > topK {
		matched = matched[:topK]
	}
	return matched
}
