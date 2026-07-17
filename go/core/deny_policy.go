package core

// Consumer-supplied deny policy — the deny-side counterpart to
// permission_grants.go. The Go port of the Rust reference
// `smooth-operator-core::deny_policy` (pearl th-ab0437).
//
// The engine ships hardcoded circuit-breakers (rm -rf /, curl | sh, credential
// paths, dangerous domains — see permission.go) and an allow-only grant store
// that can upgrade an Ask. Neither can express a consumer's own "never do this"
// rules: "never touch the prod AWS profile", "the DB writer endpoint is
// off-limits, reads go to the replica", "no writes under /prod". This file adds
// that missing tier.
//
// It is purely additive: a gate with no deny policy attached behaves exactly as
// before. When a policy is attached it is evaluated FIRST, and a match is a hard
// deny of the same tier as the built-in circuit-breakers — no stored grant
// waives it, and AutoModeBypass / AutoModeAcceptEdits cannot downgrade it.
//
// Two tiers:
//  1. Declarative (DenyRules) — TOML, four deny lists (tools/bash/network/paths).
//  2. Predicate (DenyPredicate) — a consumer-supplied interface for semantic
//     checks the engine cannot parse from strings ("is this the prod account?").
//
// Both run on every gated tool call; declarative first, then predicates. The
// first match wins.

import (
	"strings"

	"github.com/BurntSushi/toml"
)

// ---------------------------------------------------------------------------
// Declarative rules (TOML)
// ---------------------------------------------------------------------------

// DenyRules is the declarative half of a DenyPolicy: four deny lists from TOML.
type DenyRules struct {
	SchemaVersion int         `toml:"schema_version"`
	Tools         ToolsDeny   `toml:"tools,omitempty"`
	Bash          BashDeny    `toml:"bash,omitempty"`
	Network       NetworkDeny `toml:"network,omitempty"`
	Paths         PathsDeny   `toml:"paths,omitempty"`
}

// ToolsDeny is `[tools]` — deny by tool name / glob.
type ToolsDeny struct {
	Deny []string `toml:"deny,omitempty"`
}

// BashDeny is `[bash]` — deny bash command prefixes / globs.
type BashDeny struct {
	DenyPatterns []string `toml:"deny_patterns,omitempty"`
}

// NetworkDeny is `[network]` — deny host suffixes / globs.
type NetworkDeny struct {
	DenyHosts []string `toml:"deny_hosts,omitempty"`
}

// PathsDeny is `[paths]` — deny file paths / globs (Write + Read tools).
type PathsDeny struct {
	Deny []string `toml:"deny,omitempty"`
}

// NewDenyRules returns empty rules pinned at the current schema version.
func NewDenyRules() DenyRules {
	return DenyRules{SchemaVersion: 1}
}

// IsEmpty reports whether no rules are set in any section.
func (r DenyRules) IsEmpty() bool {
	return len(r.Tools.Deny) == 0 && len(r.Bash.DenyPatterns) == 0 &&
		len(r.Network.DenyHosts) == 0 && len(r.Paths.Deny) == 0
}

// ParseDenyRules parses rules from a TOML string. Missing sections default empty.
func ParseDenyRules(tomlText string) (DenyRules, error) {
	var r DenyRules
	if _, err := toml.Decode(tomlText, &r); err != nil {
		return DenyRules{}, err
	}
	return r, nil
}

// ToTOMLString serializes to TOML.
func (r DenyRules) ToTOMLString() (string, error) {
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(r); err != nil {
		return "", err
	}
	return b.String(), nil
}

// denyReason returns the first declarative rule this call matches, formatted as
// a deny reason. Empty string + false when nothing matches.
func (r DenyRules) denyReason(name string, args map[string]any) (string, bool) {
	// [tools] applies to ANY tool, whatever its category.
	for _, pat := range r.Tools.Deny {
		if globMatch(pat, name) {
			return "denied by policy (tools): " + pat, true
		}
	}
	switch toolCategory(name) {
	case categoryBash:
		cmd := strings.TrimSpace(argStr(args, "cmd", "command"))
		if cmd == "" {
			return "", false
		}
		if pat, ok := r.bashDenied(cmd); ok {
			return "denied by policy (bash): " + pat, true
		}
		// A denied host referenced by the command line is also blocked.
		for _, sub := range splitCompound(cmd) {
			for _, host := range extractHosts(sub) {
				if pat, ok := r.hostDenied(host); ok {
					return "denied by policy (network): " + pat, true
				}
			}
		}
		return "", false
	case categoryNetwork:
		raw := argStr(args, "url", "host")
		host := hostFromToken(raw)
		if host == "" {
			host = raw
		}
		if host == "" {
			return "", false
		}
		if pat, ok := r.hostDenied(host); ok {
			return "denied by policy (network): " + pat, true
		}
		return "", false
	case categoryWrite, categorySafe:
		for _, key := range []string{"path", "file", "dir", "directory"} {
			if v := argStr(args, key); v != "" {
				for _, pat := range r.Paths.Deny {
					if globMatch(pat, v) {
						return "denied by policy (paths): " + pat, true
					}
				}
			}
		}
		return "", false
	default: // categoryUnknown
		return "", false
	}
}

// bashDenied returns the first [bash] pattern that matches any (wrapper/sudo-
// stripped) subcommand.
func (r DenyRules) bashDenied(cmd string) (string, bool) {
	var subs []string
	for _, s := range splitCompound(cmd) {
		subs = append(subs, strings.ToLower(stripWrappersAndSudo(s)))
	}
	for _, pat := range r.Bash.DenyPatterns {
		// A plain prefix ("aws ") gets an implicit trailing *; a pattern with an
		// explicit * ("aws * --profile prod") also matches any trailing text so
		// extra flags don't slip a call past the rule.
		lower := strings.ToLower(pat)
		anchored := lower
		if !strings.HasSuffix(lower, "*") {
			anchored = lower + "*"
		}
		for _, sub := range subs {
			if globMatch(anchored, sub) {
				return pat, true
			}
		}
	}
	return "", false
}

// hostDenied returns the first [network] pattern that matches host (case-insensitive).
func (r DenyRules) hostDenied(host string) (string, bool) {
	h := strings.ToLower(host)
	for _, pat := range r.Network.DenyHosts {
		if hostPatternMatches(pat, h) {
			return pat, true
		}
	}
	return "", false
}

// hostPatternMatches matches a single host deny pattern against an
// already-lowercased host.
//   - no * → subdomain-aware suffix match (prod.internal ⇒ api.prod.internal).
//   - *.suffix → apex + subdomains of suffix.
//   - mid-string * (prod-*.rds.amazonaws.com) → anchored glob.
func hostPatternMatches(pattern, hostLower string) bool {
	p := strings.ToLower(pattern)
	if !strings.Contains(p, "*") {
		return domainMatchesSuffixList(hostLower, []string{p})
	}
	if bare, ok := strings.CutPrefix(p, "*."); ok {
		if domainMatchesSuffixList(hostLower, []string{bare}) {
			return true
		}
	}
	return globMatch(p, hostLower)
}

// globMatch is a minimal both-ends-anchored glob: * (and any run of *, so ** too)
// matches any sequence of characters, including /. No ?, no char classes — deny
// globs don't need them, and a tiny matcher stays auditable for a
// security-critical path.
func globMatch(pattern, text string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == text // no wildcard → exact match
	}
	first := parts[0]
	if !strings.HasPrefix(text, first) {
		return false
	}
	pos := len(first)
	lastIdx := len(parts) - 1
	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if part == "" {
			continue // consecutive/trailing *
		}
		if i == lastIdx {
			// Last literal segment must sit at the very end, and must not overlap
			// the region already consumed by earlier segments.
			endStart := len(text) - len(part)
			if endStart < 0 {
				return false
			}
			return endStart >= pos && strings.HasSuffix(text, part)
		}
		idx := strings.Index(text[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}
	// Pattern ended with * (last part empty): the trailing run matches anything.
	return true
}

// ---------------------------------------------------------------------------
// Predicate tier
// ---------------------------------------------------------------------------

// DenyReason is why a DenyPredicate blocks a call. A thin wrapper over string so
// the predicate contract is explicit and can grow structured fields later.
type DenyReason struct {
	Reason string
}

// NewDenyReason builds a DenyReason.
func NewDenyReason(reason string) DenyReason {
	return DenyReason{Reason: reason}
}

// DenyPredicate is a consumer-supplied semantic deny check. Runs on every gated
// tool call; an ok=true return is a hard deny (circuit-breaker tier). Use it for
// checks the declarative rules can't express from strings alone — resolving an
// AWS call to its account, a DB URL to writer-vs-replica, etc.
type DenyPredicate interface {
	// Evaluate returns (reason, true) to deny the call, or (_, false) to let it
	// fall through to the rest of the permission engine.
	Evaluate(name string, args map[string]any) (DenyReason, bool)
}

// ---------------------------------------------------------------------------
// The assembled policy
// ---------------------------------------------------------------------------

// DenyPolicy is a consumer-supplied deny policy: declarative rules + predicate
// checks. Attach to the gate via PermissionGate.DenyPolicy or the agent options.
// An empty policy is a no-op.
type DenyPolicy struct {
	declarative DenyRules
	predicates  []DenyPredicate
}

// NewDenyPolicy returns an empty policy — denies nothing (the additive no-op default).
func NewDenyPolicy() *DenyPolicy {
	return &DenyPolicy{}
}

// DenyPolicyFromTOML builds the declarative half from a TOML string. Predicates
// are added separately via WithPredicate.
func DenyPolicyFromTOML(tomlText string) (*DenyPolicy, error) {
	rules, err := ParseDenyRules(tomlText)
	if err != nil {
		return nil, err
	}
	return &DenyPolicy{declarative: rules}, nil
}

// WithDeclarative replaces the declarative rules. Chainable.
func (p *DenyPolicy) WithDeclarative(rules DenyRules) *DenyPolicy {
	p.declarative = rules
	return p
}

// WithPredicate adds a consumer predicate. Chainable.
func (p *DenyPolicy) WithPredicate(predicate DenyPredicate) *DenyPolicy {
	p.predicates = append(p.predicates, predicate)
	return p
}

// IsEmpty reports whether there are no rules and no predicates.
func (p *DenyPolicy) IsEmpty() bool {
	return p.declarative.IsEmpty() && len(p.predicates) == 0
}

// Evaluate returns the deny reason for a call, or ("", false) to let it fall
// through to the rest of the permission engine. Declarative rules are checked
// first, then predicates; the first match wins.
func (p *DenyPolicy) Evaluate(name string, args map[string]any) (string, bool) {
	if reason, ok := p.declarative.denyReason(name, args); ok {
		return reason, true
	}
	for _, predicate := range p.predicates {
		if r, ok := predicate.Evaluate(name, args); ok {
			return "denied by policy (predicate): " + r.Reason, true
		}
	}
	return "", false
}
