package core

// Persistent permission grants — wonk-allow.toml. The Go port of the Rust
// reference `smooth-operator-core::permission_grants` (pearl th-ab0437).
//
// The permission gate closes on an Ask verdict by prompting a human. Without
// persistence that prompt is approve-once: the same command re-asks on every
// run. This file ports smooth's wonk-allow.toml allow-list so a human's "approve
// always" answer is remembered — a stored grant that matches a later Ask
// auto-approves it without prompting.
//
// Two TOML files are stacked at load time (project wins on collision):
//   - ~/.smooth/wonk-allow.toml            — the user's personal grants.
//   - <repo>/.smooth/wonk-allow.toml       — project-scoped grants (checked in).
//
// Schema (v1):
//
//	schema_version = 1
//	[network]
//	allow_hosts = ["api.openai.com", "*.openai.com"]
//	[tools]
//	allow = ["web_search", "vendor.file_write"]
//	[bash]
//	allow_patterns = ["cargo ", "pnpm "]
//
// There is no deny section: a stored grant can only upgrade an Ask, never waive
// a Deny circuit-breaker (deny rules live in deny_policy.go).

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// PermissionGrants is the in-memory snapshot of wonk-allow.toml.
type PermissionGrants struct {
	SchemaVersion int            `toml:"schema_version"`
	Network       NetworkSection `toml:"network,omitempty"`
	Tools         ToolsSection   `toml:"tools,omitempty"`
	Bash          BashSection    `toml:"bash,omitempty"`
}

// NetworkSection holds hosts (or *.suffix globs) approved without asking.
type NetworkSection struct {
	AllowHosts []string `toml:"allow_hosts,omitempty"`
}

// ToolsSection holds tool names approved without asking. Exact match only.
type ToolsSection struct {
	Allow []string `toml:"allow,omitempty"`
}

// BashSection holds command prefixes approved without asking. "cargo " matches
// `cargo test`, `cargo build`, … — the trailing space guards against `cargonaut`.
type BashSection struct {
	AllowPatterns []string `toml:"allow_patterns,omitempty"`
}

// GrantKind tags a GrantQuery.
type GrantKind int

const (
	// GrantNetwork is a network host (or *.suffix glob).
	GrantNetwork GrantKind = iota
	// GrantTool is an exact tool name (write / unknown tool).
	GrantTool
	// GrantBash is a bash command prefix, e.g. "npm ".
	GrantBash
)

// GrantQuery is the kind of resource a grant covers — one of the three grantable
// Ask shapes. (Deny circuit-breakers are never grantable.)
type GrantQuery struct {
	Kind  GrantKind
	Value string
}

// NewPermissionGrants returns grants pinned at the current schema version.
func NewPermissionGrants() *PermissionGrants {
	return &PermissionGrants{SchemaVersion: 1}
}

// MatchesHost reports whether host is covered by the [network] allow-list.
func (g *PermissionGrants) MatchesHost(host string) bool {
	lower := strings.ToLower(host)
	for _, pat := range g.Network.AllowHosts {
		if hostMatchesGlob(lower, pat) {
			return true
		}
	}
	return false
}

// MatchesTool reports whether toolName is in the [tools] allow-list (exact match).
func (g *PermissionGrants) MatchesTool(toolName string) bool {
	for _, t := range g.Tools.Allow {
		if t == toolName {
			return true
		}
	}
	return false
}

// MatchesBash reports whether command starts with any [bash] allow prefix.
func (g *PermissionGrants) MatchesBash(command string) bool {
	lower := strings.ToLower(command)
	for _, p := range g.Bash.AllowPatterns {
		if strings.HasPrefix(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

// Contains reports whether query's exact entry is already stored.
func (g *PermissionGrants) Contains(query GrantQuery) bool {
	switch query.Kind {
	case GrantNetwork:
		return g.MatchesHost(query.Value)
	case GrantTool:
		return g.MatchesTool(query.Value)
	case GrantBash:
		return g.MatchesBash(query.Value)
	}
	return false
}

// Add adds a grant. Idempotent; keeps each section sorted+unique for stable output.
func (g *PermissionGrants) Add(query GrantQuery) {
	switch query.Kind {
	case GrantNetwork:
		g.Network.AllowHosts = insertSorted(g.Network.AllowHosts, query.Value)
	case GrantTool:
		g.Tools.Allow = insertSorted(g.Tools.Allow, query.Value)
	case GrantBash:
		g.Bash.AllowPatterns = insertSorted(g.Bash.AllowPatterns, query.Value)
	}
}

func insertSorted(list []string, v string) []string {
	for _, e := range list {
		if e == v {
			return list
		}
	}
	list = append(list, v)
	sort.Strings(list)
	return list
}

// MergeWith unions other into g.
func (g *PermissionGrants) MergeWith(other *PermissionGrants) {
	if other.SchemaVersion > g.SchemaVersion {
		g.SchemaVersion = other.SchemaVersion
	}
	for _, h := range other.Network.AllowHosts {
		g.Network.AllowHosts = insertSorted(g.Network.AllowHosts, h)
	}
	for _, t := range other.Tools.Allow {
		g.Tools.Allow = insertSorted(g.Tools.Allow, t)
	}
	for _, p := range other.Bash.AllowPatterns {
		g.Bash.AllowPatterns = insertSorted(g.Bash.AllowPatterns, p)
	}
}

// Clone returns a deep copy.
func (g *PermissionGrants) Clone() *PermissionGrants {
	cp := &PermissionGrants{SchemaVersion: g.SchemaVersion}
	cp.Network.AllowHosts = append([]string(nil), g.Network.AllowHosts...)
	cp.Tools.Allow = append([]string(nil), g.Tools.Allow...)
	cp.Bash.AllowPatterns = append([]string(nil), g.Bash.AllowPatterns...)
	return cp
}

// ParsePermissionGrants parses grants from a TOML string. Missing sections
// default to empty.
func ParsePermissionGrants(tomlText string) (*PermissionGrants, error) {
	var g PermissionGrants
	if _, err := toml.Decode(tomlText, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// ToTOMLString serializes to TOML.
func (g *PermissionGrants) ToTOMLString() (string, error) {
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(g); err != nil {
		return "", err
	}
	return b.String(), nil
}

// LoadPermissionGrants loads from path. A missing file yields an empty (v1) store
// — NOT an error. A malformed file surfaces the parse error.
func LoadPermissionGrants(path string) (*PermissionGrants, error) {
	text, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return NewPermissionGrants(), nil
		}
		return nil, err
	}
	g, err := ParsePermissionGrants(string(text))
	if err != nil {
		return nil, fmt.Errorf("malformed wonk-allow.toml at %s: %w", path, err)
	}
	return g, nil
}

// LoadLayeredGrants loads user + project files and merges them (project last so
// its schema_version wins; entries union either way). Either path empty is fine;
// a malformed file present is an error.
func LoadLayeredGrants(userPath, projectPath string) (*PermissionGrants, error) {
	merged := NewPermissionGrants()
	if userPath != "" {
		u, err := LoadPermissionGrants(userPath)
		if err != nil {
			return nil, err
		}
		merged.MergeWith(u)
	}
	if projectPath != "" {
		p, err := LoadPermissionGrants(projectPath)
		if err != nil {
			return nil, err
		}
		merged.MergeWith(p)
	}
	return merged, nil
}

// SaveToPath atomically writes g to path (tempfile + rename), creating parent dirs.
func (g *PermissionGrants) SaveToPath(path string) error {
	if parent := filepath.Dir(path); parent != "" {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	text, err := g.ToTOMLString()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// UserGrantsPath is the user-scope grants file: ~/.smooth/wonk-allow.toml. Empty
// when there is no home dir (minimal CI / broken containers).
func UserGrantsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".smooth", "wonk-allow.toml")
}

// ProjectGrantsPath is the project-scope grants file: <workspace>/.smooth/wonk-allow.toml.
func ProjectGrantsPath(workspace string) string {
	return filepath.Join(workspace, ".smooth", "wonk-allow.toml")
}

// AppendGrant loads the grant at path, adds query, and atomically saves. Creates
// the file if absent. Idempotent for a query that's already stored.
func AppendGrant(path string, query GrantQuery) error {
	grants, err := LoadPermissionGrants(path)
	if err != nil {
		return err
	}
	if grants.SchemaVersion == 0 {
		grants.SchemaVersion = 1
	}
	grants.Add(query)
	return grants.SaveToPath(path)
}

// SharedGrants is a thread-safe, cheaply-shared handle to the live merged grants.
// Reads take a snapshot; approve-always merges the freshly-persisted grant back in.
type SharedGrants struct {
	mu    sync.RWMutex
	inner *PermissionGrants
}

// NewSharedGrants wraps grants for concurrent access.
func NewSharedGrants(grants *PermissionGrants) *SharedGrants {
	if grants == nil {
		grants = NewPermissionGrants()
	}
	return &SharedGrants{inner: grants}
}

// Snapshot returns a cloned-out copy for lock-free matching.
func (s *SharedGrants) Snapshot() *PermissionGrants {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inner.Clone()
}

// MergeIn unions other into the live grants.
func (s *SharedGrants) MergeIn(other *PermissionGrants) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inner.MergeWith(other)
}

// hostMatchesGlob is a glob match for a single host pattern (case-insensitive):
//   - exact host: api.example.com matches only that.
//   - *.example.com / .example.com: any subdomain AND the bare apex.
//   - a bare suffix (example.com) matches only itself (no substring match, so
//     evil-example.com never slips past example.com).
func hostMatchesGlob(host, pattern string) bool {
	h := strings.ToLower(host)
	p := strings.ToLower(pattern)
	if h == p {
		return true
	}
	if suffix, ok := strings.CutPrefix(p, "*."); ok {
		return strings.HasSuffix(h, "."+suffix) || h == suffix
	}
	if suffix, ok := strings.CutPrefix(p, "."); ok {
		return strings.HasSuffix(h, "."+suffix) || h == suffix
	}
	return false
}
