package core

// Prompt caching — the static/dynamic system-prompt split. The Go port of the
// Rust reference `smooth-operator-core::conversation::PromptCache`.
//
// A system prompt has two halves with very different churn rates: role
// instructions and tool schemas barely change, while project context
// (AGENTS.md / CLAUDE.md, the working set) changes every turn. Anthropic's
// prompt cache keys on a PREFIX, so putting the volatile half first invalidates
// the whole thing. PromptCacheBoundary splits them: everything above the marker
// is static and hashed once for cache-key dedup, everything below is dynamic and
// can be swapped without busting the static prefix.
//
// This is the caller-facing half of prompt caching. The wire half — attaching
// `cache_control: {"type":"ephemeral"}` to the strategic prefix boundaries — is
// in openai.go.

import (
	"fmt"
	"strings"
)

// PromptCacheBoundary splits a system prompt into a cacheable static portion
// and a frequently-changing dynamic portion.
const PromptCacheBoundary = "__PROMPT_CACHE_BOUNDARY__"

// PromptCache is a system prompt split at [PromptCacheBoundary].
type PromptCache struct {
	staticPortion  string
	dynamicPortion string
	staticHash     string
	staticTokens   int
}

// NewPromptCache splits a system prompt at the boundary marker. With no marker
// the entire prompt is treated as dynamic — nothing is claimed to be cacheable
// that the caller didn't mark.
func NewPromptCache(prompt string) *PromptCache {
	staticPortion, dynamicPortion := "", prompt
	if idx := strings.Index(prompt, PromptCacheBoundary); idx >= 0 {
		staticPortion = prompt[:idx]
		if after := idx + len(PromptCacheBoundary); after < len(prompt) {
			dynamicPortion = prompt[after:]
		} else {
			dynamicPortion = ""
		}
	}

	tokens := 0
	if staticPortion != "" {
		tokens = len(staticPortion)/4 + 1
	}
	return &PromptCache{
		staticPortion:  staticPortion,
		dynamicPortion: dynamicPortion,
		staticHash:     hashPromptPortion(staticPortion),
		staticTokens:   tokens,
	}
}

// StaticPortion is the cacheable half (above the marker).
func (p *PromptCache) StaticPortion() string { return p.staticPortion }

// DynamicPortion is the frequently-changing half (below the marker).
func (p *PromptCache) DynamicPortion() string { return p.dynamicPortion }

// FullPrompt reassembles static + boundary + dynamic. With no static portion
// the dynamic half is returned alone, so a prompt that was never split round-
// trips unchanged rather than gaining a stray marker.
func (p *PromptCache) FullPrompt() string {
	if p.staticPortion == "" {
		return p.dynamicPortion
	}
	return p.staticPortion + PromptCacheBoundary + p.dynamicPortion
}

// UpdateDynamic swaps the dynamic half, leaving the static half and its hash
// untouched — the whole point of the split.
func (p *PromptCache) UpdateDynamic(dynamic string) { p.dynamicPortion = dynamic }

// StaticHash identifies the static portion for cache-key deduplication.
//
// Process-local only: it is compared against other hashes from THIS engine,
// never sent on the wire, so it deliberately does not match the Rust
// reference's value (Rust uses DefaultHasher, which is not reproducible across
// languages — or even across Rust releases). The contract that is ported is the
// behavior: same static text hashes the same, different static text hashes
// differently, and UpdateDynamic never changes it.
func (p *PromptCache) StaticHash() string { return p.staticHash }

// CachedTokens estimates the tokens the static portion saves on a cache hit.
func (p *PromptCache) CachedTokens() int { return p.staticTokens }

// hashPromptPortion is FNV-1a (64-bit), the same non-cryptographic hash the
// vector embedder uses, rendered as 16 hex chars like the Rust reference.
func hashPromptPortion(s string) string {
	const (
		offset64 uint64 = 14695981039346656037
		prime64  uint64 = 1099511628211
	)
	h := offset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return fmt.Sprintf("%016x", h)
}
