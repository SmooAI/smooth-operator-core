package core

// Project context loader — AGENTS.md (or its fallbacks) plus resolved file
// references. The Go port of the Rust reference `smooth-operator-core::context`
// (pearl th-5002c4).
//
// Smooth previously only read AGENTS.md. Many projects don't have one but DO
// have CLAUDE.md or a SMOOTH.md or .smooth/CONTEXT.md. User-level facts also
// belong in the prompt, so we walk a preference order and STACK user-level +
// project-level context.
//
// Preference order (first hit per layer; layers stack):
//
//   - USER layer (read once, prepended):
//     ~/.smooth/CONTEXT.md → ~/.smooth/AGENTS.md → ~/.smooth/CLAUDE.md
//
//   - PROJECT layer (walk up from workingDir, first hit wins):
//     <dir>/.smooth/CONTEXT.md → <dir>/SMOOTH.md → <dir>/AGENTS.md → <dir>/CLAUDE.md
//
// AGENTS.md / SMOOTH.md can carry file references in a `## File References`
// section:
//
//	## File References
//	- [CLAUDE.md](CLAUDE.md) — full file
//	- [Section name](CLAUDE.md#6-pearl-tracking) — specific section
//
// Those are resolved against the file's directory and appended inline. The
// combined string is what the host injects into the agent's system prompt —
// like the Rust reference, this is a standalone loader, NOT a hook inside the
// agent loop.

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// FileRef is a parsed file reference from the `## File References` section.
// Fragment and Description are nil when absent (Rust's `Option<String>`).
type FileRef struct {
	// Label is the display label from the markdown link text.
	Label string
	// Path is the relative file path (without fragment).
	Path string
	// Fragment optionally points at a heading within Path.
	Fragment *string
	// Description is the optional text after the ` — `.
	Description *string
}

var userContextCandidates = []string{
	filepath.Join(".smooth", "CONTEXT.md"),
	filepath.Join(".smooth", "AGENTS.md"),
	filepath.Join(".smooth", "CLAUDE.md"),
}

var projectContextCandidates = []string{
	filepath.Join(".smooth", "CONTEXT.md"),
	"SMOOTH.md",
	"AGENTS.md",
	"CLAUDE.md",
}

// LoadProjectContext loads the combined project + user context, user-level
// prepended, with file references in any AGENTS.md / SMOOTH.md resolved inline.
//
// The bool is false only when NEITHER layer found anything — so a workspace
// with a bare CLAUDE.md and no user-level file still loads context.
func LoadProjectContext(workingDir string) (string, bool) {
	user, hasUser := loadUserContext()
	project, hasProject := loadLayeredProjectContext(workingDir)

	switch {
	case !hasUser && !hasProject:
		return "", false
	case hasUser && !hasProject:
		return "## User context (~/.smooth)\n\n" + user, true
	case !hasUser && hasProject:
		return project, true
	default:
		return "## User context (~/.smooth)\n\n" + user + "\n\n---\n\n" + project, true
	}
}

// loadUserContext walks the user-level preference list and returns the first
// non-blank hit. False when the user has no ~/.smooth context at all.
func loadUserContext() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	for _, candidate := range userContextCandidates {
		raw, err := os.ReadFile(filepath.Join(home, candidate))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(raw)) != "" {
			return string(raw), true
		}
	}
	return "", false
}

// loadLayeredProjectContext finds the project context file and returns it with
// its file references resolved.
func loadLayeredProjectContext(workingDir string) (string, bool) {
	contextPath, ok := findProjectContextFile(workingDir)
	if !ok {
		return "", false
	}
	rawBytes, err := os.ReadFile(contextPath)
	if err != nil {
		return "", false
	}
	raw := string(rawBytes)

	refs := ParseFileReferences(raw)
	if len(refs) == 0 {
		return raw, true
	}

	resolved := resolveReferences(filepath.Dir(contextPath), refs)
	if len(resolved) == 0 {
		return raw, true
	}

	var b strings.Builder
	b.WriteString(raw)
	b.WriteString("\n---\n\n## Resolved File References\n\n")
	for _, r := range resolved {
		if r.ref.Description != nil {
			b.WriteString("### " + r.ref.Label + " — " + *r.ref.Description + "\n")
		} else {
			b.WriteString("### " + r.ref.Label + "\n")
		}
		b.WriteString("\n```\n")
		b.WriteString(r.content)
		if !strings.HasSuffix(r.content, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("```\n\n")
	}
	return b.String(), true
}

// findProjectContextFile walks up from startDir. Preference order at each
// level: .smooth/CONTEXT.md → SMOOTH.md → AGENTS.md → CLAUDE.md. First hit
// wins per directory, then keep walking up until one is found.
func findProjectContextFile(startDir string) (string, bool) {
	dir := startDir
	for {
		for _, candidate := range projectContextCandidates {
			path := filepath.Join(dir, candidate)
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
				return path, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// ParseFileReferences parses the `## File References` section out of AGENTS.md
// content. Expects markdown list items like:
//
//   - [Label](path.md) — description
//   - [Label](path.md#fragment) — description
func ParseFileReferences(content string) []FileRef {
	refs := []FileRef{}
	inSection := false

	for _, line := range splitLines(content) {
		trimmed := strings.TrimSpace(line)

		// Detect the file references section.
		if strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "# ") {
			inSection = strings.Contains(strings.ToLower(trimmed), "file reference")
			continue
		}
		if !inSection {
			continue
		}
		if ref, ok := parseLinkLine(trimmed); ok {
			refs = append(refs, ref)
		}
	}
	return refs
}

// parseLinkLine parses a single markdown list-item link line.
func parseLinkLine(line string) (FileRef, bool) {
	// Strip leading `- ` or `* `.
	rest, ok := strings.CutPrefix(line, "- ")
	if !ok {
		rest, ok = strings.CutPrefix(line, "* ")
		if !ok {
			return FileRef{}, false
		}
	}
	line = rest

	// Match [label](target).
	openBracket := strings.Index(line, "[")
	if openBracket < 0 {
		return FileRef{}, false
	}
	rel := strings.Index(line[openBracket:], "]")
	if rel < 0 {
		return FileRef{}, false
	}
	closeBracket := rel + openBracket
	label := line[openBracket+1 : closeBracket]

	after := line[closeBracket+1:]
	openParen := strings.Index(after, "(")
	if openParen < 0 {
		return FileRef{}, false
	}
	rel = strings.Index(after[openParen:], ")")
	if rel < 0 {
		return FileRef{}, false
	}
	closeParen := rel + openParen
	target := after[openParen+1 : closeParen]

	// Split path and fragment.
	path := target
	var fragment *string
	if hash := strings.Index(target, "#"); hash >= 0 {
		path = target[:hash]
		f := target[hash+1:]
		fragment = &f
	}

	// Description after ` — ` / ` - ` / ` -- `.
	var description *string
	afterLink := after[closeParen+1:]
	for _, sep := range []string{" — ", " - ", " -- "} {
		if d, ok := strings.CutPrefix(afterLink, sep); ok {
			if d = strings.TrimSpace(d); d != "" {
				description = &d
			}
			break
		}
	}

	if path == "" && fragment == nil {
		return FileRef{}, false
	}
	return FileRef{Label: label, Path: path, Fragment: fragment, Description: description}, true
}

type resolvedRef struct {
	ref     FileRef
	content string
}

// resolveReferences resolves file references against a base directory,
// skipping any that are unreadable or resolve to nothing.
func resolveReferences(baseDir string, refs []FileRef) []resolvedRef {
	results := []resolvedRef{}
	for _, ref := range refs {
		raw, err := os.ReadFile(filepath.Join(baseDir, ref.Path))
		if err != nil {
			continue // Skip unreadable files.
		}
		content := string(raw)
		if ref.Fragment != nil {
			content = extractSection(content, *ref.Fragment)
		}
		if strings.TrimSpace(content) != "" {
			results = append(results, resolvedRef{ref: ref, content: content})
		}
	}
	return results
}

// extractSection extracts a markdown section by heading fragment. The fragment
// is matched against GitHub-style heading anchors; the section runs until the
// next heading at the same or a higher level.
func extractSection(content, fragment string) string {
	target := headingToAnchor(fragment)
	lines := splitLines(content)
	start := -1
	startLevel := 0

	for i, line := range lines {
		level, text, ok := parseHeading(line)
		if !ok {
			continue
		}
		anchor := headingToAnchor(text)
		if anchor == target || strings.Contains(anchor, target) || strings.Contains(target, anchor) {
			start = i
			startLevel = level
			continue
		}
		// Started capturing and hit a same-or-higher-level heading: stop.
		if start >= 0 && level <= startLevel {
			return strings.Join(lines[start:i], "\n")
		}
	}

	if start >= 0 {
		return strings.Join(lines[start:], "\n")
	}
	return ""
}

// parseHeading parses a markdown heading line, returning (level, text).
func parseHeading(line string) (int, string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "#") {
		return 0, "", false
	}
	level := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
	text := strings.TrimSpace(trimmed[level:])
	if text == "" {
		return 0, "", false
	}
	return level, text, true
}

// headingToAnchor converts heading text to a GitHub-style anchor.
func headingToAnchor(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('-')
		}
		// Other characters are dropped.
	}
	// Single non-overlapping pass, matching Rust's `str::replace`.
	return strings.ReplaceAll(b.String(), "--", "-")
}

// splitLines mirrors Rust's `str::lines`: split on \n, drop a trailing \r on
// each line, and do not emit a trailing empty line for a final newline.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	parts := strings.Split(s, "\n")
	for i, p := range parts {
		parts[i] = strings.TrimSuffix(p, "\r")
	}
	return parts
}
