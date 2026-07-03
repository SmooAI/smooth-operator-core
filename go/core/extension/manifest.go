package extension

// Extension manifests — extension.toml discovery, merge, and ${env:VAR}
// expansion. Mirrors the MCP config pattern:
//
//   - An extension lives in a directory holding an extension.toml.
//   - Global extensions: ~/.smooth/extensions/<name>/extension.toml.
//   - Project extensions: <workspace>/.smooth/extensions/<name>/extension.toml.
//   - On a name collision the PROJECT entry wins.
//   - [run] env values support ${env:VAR} expansion so secrets stay out of the manifest.
//   - A single malformed manifest is tolerated: it is collected as a failure and
//     the rest still load.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Scope records where a manifest was discovered. Project extensions only load in
// trusted workspaces; the host uses this to apply that policy.
type Scope int

const (
	ScopeGlobal Scope = iota
	ScopeProject
)

func (s Scope) String() string {
	if s == ScopeProject {
		return "project"
	}
	return "global"
}

// RunSpec is how to launch the extension subprocess.
type RunSpec struct {
	// Command is the executable to spawn (e.g. node, python3, an absolute path).
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	// Env holds extra env vars; values may reference ${env:VAR}.
	Env map[string]string `toml:"env"`
}

// Capabilities declares what the extension can do. The Events list doubles as the
// host's dispatch filter — an extension only receives events it names here.
type Capabilities struct {
	Events   []string `toml:"events"`
	Tools    bool     `toml:"tools"`
	Commands bool     `toml:"commands"`
	UI       bool     `toml:"ui"`
	Exec     bool     `toml:"exec"`
	KV       bool     `toml:"kv"`
	Bus      bool     `toml:"bus"`
	Session  bool     `toml:"session"`
}

// Resources are directories the extension contributes (skills, prompts, themes).
type Resources struct {
	Skills  string `toml:"skills"`
	Prompts string `toml:"prompts"`
	Themes  string `toml:"themes"`
}

// ExtensionManifest is a parsed extension.toml.
type ExtensionManifest struct {
	Name    string `toml:"name"`
	Version string `toml:"version"`
	// Protocol is the highest SEP protocol version the extension declares (default 1).
	Protocol     int          `toml:"protocol"`
	Run          RunSpec      `toml:"run"`
	Capabilities Capabilities `toml:"capabilities"`
	Resources    Resources    `toml:"resources"`
	// HookTimeoutMS is a per-extension hook timeout override, in milliseconds (0 = default).
	HookTimeoutMS int64 `toml:"hook_timeout_ms"`
	// Disabled skips the extension without deleting its manifest.
	Disabled bool `toml:"disabled"`
}

// ParseManifest parses a manifest from TOML text. It enforces the required
// fields (name, version, run.command) so a malformed manifest is rejected rather
// than silently defaulting.
func ParseManifest(tomlText string) (ExtensionManifest, error) {
	var m ExtensionManifest
	if _, err := toml.Decode(tomlText, &m); err != nil {
		return ExtensionManifest{}, fmt.Errorf("parse extension.toml: %w", err)
	}
	if m.Protocol == 0 {
		m.Protocol = 1
	}
	if m.Name == "" {
		return ExtensionManifest{}, fmt.Errorf("parse extension.toml: name is required")
	}
	if m.Version == "" {
		return ExtensionManifest{}, fmt.Errorf("parse extension.toml: version is required")
	}
	if m.Run.Command == "" {
		return ExtensionManifest{}, fmt.Errorf("parse extension.toml: [run] command is required")
	}
	return m, nil
}

// loadManifestDir loads a manifest from <dir>/extension.toml.
func loadManifestDir(dir string) (ExtensionManifest, error) {
	path := filepath.Join(dir, "extension.toml")
	text, err := os.ReadFile(path)
	if err != nil {
		return ExtensionManifest{}, fmt.Errorf("read %s: %w", path, err)
	}
	return ParseManifest(string(text))
}

// ResolvedEnv returns the [run] env map with ${env:VAR} references expanded
// against the host's current environment. Unset variables expand to empty strings.
func (m ExtensionManifest) ResolvedEnv() map[string]string {
	out := make(map[string]string, len(m.Run.Env))
	for k, v := range m.Run.Env {
		out[k] = expandEnv(v)
	}
	return out
}

// DiscoveredExtension is a discovered extension: its manifest plus the directory
// it was found in (relative resources and args resolve against this root) and its
// scope.
type DiscoveredExtension struct {
	Manifest ExtensionManifest
	Root     string
	Scope    Scope
}

// DefaultGlobalDir is the default global extensions directory: $SMOOTH_HOME/extensions
// if set, else ~/.smooth/extensions. Returns "" if no home dir can be resolved.
func DefaultGlobalDir() string {
	if home := os.Getenv("SMOOTH_HOME"); home != "" {
		return filepath.Join(home, "extensions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".smooth", "extensions")
}

// ProjectDir is the project extensions directory for a workspace root.
func ProjectDir(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".smooth", "extensions")
}

// DiscoveryFailure records a manifest that failed to parse.
type DiscoveryFailure struct {
	Source string
	Err    string
}

// Discover finds every extension under globalDir and projectDir, merging by name
// with PROJECT winning. Either directory may be "" or missing (treated as empty).
// Returns the chosen extensions (sorted by name for deterministic load order)
// plus a list of failures for manifests that failed to parse — a single bad
// manifest never aborts discovery.
func Discover(globalDir, projectDir string) ([]DiscoveredExtension, []DiscoveryFailure) {
	var failures []DiscoveryFailure
	byName := map[string]DiscoveredExtension{}

	// Global first, then project, so project overwrites on name collision.
	for _, d := range []struct {
		dir   string
		scope Scope
	}{{globalDir, ScopeGlobal}, {projectDir, ScopeProject}} {
		if d.dir == "" {
			continue
		}
		for _, found := range scanDir(d.dir, d.scope, &failures) {
			byName[found.Manifest.Name] = found
		}
	}

	chosen := make([]DiscoveredExtension, 0, len(byName))
	for _, e := range byName {
		chosen = append(chosen, e)
	}
	// Stable order so load-order-dependent hook chaining is deterministic.
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].Manifest.Name < chosen[j].Manifest.Name })
	return chosen, failures
}

// scanDir scans a single extensions directory: each immediate subdirectory
// holding an extension.toml is one extension.
func scanDir(dir string, scope Scope, failures *[]DiscoveryFailure) []DiscoveredExtension {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Missing dir is not an error — just no extensions from this scope.
		return nil
	}
	var out []DiscoveredExtension
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		root := filepath.Join(dir, entry.Name())
		info, err := os.Stat(filepath.Join(root, "extension.toml"))
		if err != nil || info.IsDir() {
			continue
		}
		manifest, err := loadManifestDir(root)
		if err != nil {
			*failures = append(*failures, DiscoveryFailure{Source: root, Err: err.Error()})
			continue
		}
		out = append(out, DiscoveredExtension{Manifest: manifest, Root: root, Scope: scope})
	}
	return out
}

// expandEnv expands ${env:VAR} references using the host's current environment.
// Unset variables expand to empty strings. An unterminated reference is left verbatim.
func expandEnv(input string) string {
	var b strings.Builder
	rest := input
	for {
		idx := strings.Index(rest, "${env:")
		if idx < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:idx])
		after := rest[idx+len("${env:"):]
		end := strings.IndexByte(after, '}')
		if end < 0 {
			b.WriteString(rest[idx:])
			return b.String()
		}
		b.WriteString(os.Getenv(after[:end]))
		rest = after[end+1:]
	}
}
