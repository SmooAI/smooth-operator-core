package core

// Ports the Rust reference engine's `context.rs` tests one-for-one.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestParseSimpleLink(t *testing.T) {
	r, ok := parseLinkLine("- [CLAUDE.md](CLAUDE.md) — Project overview")
	if !ok {
		t.Fatal("expected a parsed link")
	}
	if r.Label != "CLAUDE.md" || r.Path != "CLAUDE.md" {
		t.Fatalf("label/path: %+v", r)
	}
	if r.Fragment != nil {
		t.Fatalf("expected no fragment, got %q", *r.Fragment)
	}
	if r.Description == nil || *r.Description != "Project overview" {
		t.Fatalf("description: %+v", r.Description)
	}
}

func TestParseLinkWithFragment(t *testing.T) {
	r, ok := parseLinkLine("- [Pearl tracking](CLAUDE.md#6-pearl-tracking) — Pearl workflow")
	if !ok {
		t.Fatal("expected a parsed link")
	}
	if r.Label != "Pearl tracking" || r.Path != "CLAUDE.md" {
		t.Fatalf("label/path: %+v", r)
	}
	if r.Fragment == nil || *r.Fragment != "6-pearl-tracking" {
		t.Fatalf("fragment: %+v", r.Fragment)
	}
	if r.Description == nil || *r.Description != "Pearl workflow" {
		t.Fatalf("description: %+v", r.Description)
	}
}

func TestParseLinkNoDescription(t *testing.T) {
	r, ok := parseLinkLine("- [README](README.md)")
	if !ok {
		t.Fatal("expected a parsed link")
	}
	if r.Label != "README" || r.Path != "README.md" {
		t.Fatalf("label/path: %+v", r)
	}
	if r.Fragment != nil || r.Description != nil {
		t.Fatalf("expected no fragment/description: %+v", r)
	}
}

func TestParseFileReferencesSection(t *testing.T) {
	content := "# Agent Instructions\n\nSome intro text.\n\n## File References\n\n" +
		"- [CLAUDE.md](CLAUDE.md) — Full file\n" +
		"- [Testing](CLAUDE.md#8-testing) — Testing reqs\n\n" +
		"## Other Section\n\n" +
		"- [not a ref](foo.md)\n"

	refs := ParseFileReferences(content)
	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d: %+v", len(refs), refs)
	}
	if refs[0].Path != "CLAUDE.md" || refs[0].Fragment != nil {
		t.Fatalf("ref 0: %+v", refs[0])
	}
	if refs[1].Path != "CLAUDE.md" || refs[1].Fragment == nil || *refs[1].Fragment != "8-testing" {
		t.Fatalf("ref 1: %+v", refs[1])
	}
}

func TestHeadingToAnchorBasic(t *testing.T) {
	cases := map[string]string{
		"6. Pearl Tracking":   "6-pearl-tracking",
		"Testing - MANDATORY": "testing--mandatory",
		"Simple Heading":      "simple-heading",
	}
	for in, want := range cases {
		if got := headingToAnchor(in); got != want {
			t.Errorf("headingToAnchor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractSectionByFragment(t *testing.T) {
	content := "# Top\n\nIntro\n\n## Section A\n\nContent A\n\n## Section B\n\nContent B\n\n### Subsection\n\nSub content\n"
	section := extractSection(content, "section-a")
	if !strings.Contains(section, "## Section A") || !strings.Contains(section, "Content A") {
		t.Fatalf("missing Section A: %q", section)
	}
	if strings.Contains(section, "Section B") {
		t.Fatalf("leaked Section B: %q", section)
	}
}

func TestExtractSectionToEOF(t *testing.T) {
	content := "# Top\n\n## Last Section\n\nFinal content\n"
	section := extractSection(content, "last-section")
	if !strings.Contains(section, "## Last Section") || !strings.Contains(section, "Final content") {
		t.Fatalf("section: %q", section)
	}
}

func TestExtractSectionNotFound(t *testing.T) {
	if section := extractSection("# Top\n\n## Existing\n\nContent\n", "nonexistent"); section != "" {
		t.Fatalf("expected empty, got %q", section)
	}
}

func TestLoadFromTempDir(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "CLAUDE.md"),
		"# Project\n\nOverview\n\n## Testing\n\nAll tests must pass.\n\n## Deploy\n\nNever deploy locally.\n")
	writeFile(t, filepath.Join(tmp, "AGENTS.md"),
		"# Agent Instructions\n\n## File References\n\n- [Testing](CLAUDE.md#testing) — Test reqs\n\n## Rules\n\nBe helpful.\n")

	ctx, ok := LoadProjectContext(tmp)
	if !ok {
		t.Fatal("expected context")
	}
	for _, want := range []string{"Agent Instructions", "Resolved File References", "All tests must pass"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("missing %q in: %s", want, ctx)
		}
	}
}

func TestLoadReturnsNothingWhenNoContextFilesAnywhere(t *testing.T) {
	// Mirrors the Rust test's caveat: the walk-up escapes to the filesystem
	// root and the user layer can't be mocked, so assert the practical
	// guarantee — nothing loaded refers to the (empty) temp dir.
	tmp := t.TempDir()
	if ctx, ok := LoadProjectContext(tmp); ok && strings.Contains(ctx, tmp) {
		t.Fatalf("found context referring to an empty temp dir: %s", ctx)
	}
}

func TestLoadPrefersSmoothContextOverClaudeMd(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".smooth", "CONTEXT.md"), "# Smooth context\n\nthe winner")
	writeFile(t, filepath.Join(tmp, "CLAUDE.md"), "# Claude.md\n\nshould lose")

	ctx, ok := LoadProjectContext(tmp)
	if !ok {
		t.Fatal("expected context")
	}
	if !strings.Contains(ctx, "the winner") || strings.Contains(ctx, "should lose") {
		t.Fatalf(".smooth/CONTEXT.md must take precedence: %s", ctx)
	}
}

func TestLoadFallsBackToClaudeMdWhenNoAgents(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "CLAUDE.md"), "# CLAUDE.md\n\nfallback content")

	ctx, ok := LoadProjectContext(tmp)
	if !ok || !strings.Contains(ctx, "fallback content") {
		t.Fatalf("should fall back to CLAUDE.md: %s", ctx)
	}
}

func TestLoadPrefersSmoothMdOverClaudeMd(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "SMOOTH.md"), "# SMOOTH.md\n\nsmooth wins")
	writeFile(t, filepath.Join(tmp, "CLAUDE.md"), "# CLAUDE.md\n\nclaude loses")

	ctx, ok := LoadProjectContext(tmp)
	if !ok {
		t.Fatal("expected context")
	}
	if !strings.Contains(ctx, "smooth wins") || strings.Contains(ctx, "claude loses") {
		t.Fatalf("SMOOTH.md must win: %s", ctx)
	}
}

func TestFindProjectContextWalksUp(t *testing.T) {
	tmp := t.TempDir()
	nested := filepath.Join(tmp, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tmp, "CLAUDE.md"), "# CLAUDE.md at root")

	found, ok := findProjectContextFile(nested)
	if !ok || !strings.HasSuffix(found, "CLAUDE.md") {
		t.Fatalf("expected to walk up to CLAUDE.md, got %q (ok=%v)", found, ok)
	}
}

func TestLoadWithoutFileReferencesReturnsRaw(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "AGENTS.md"), "# Agent Instructions\n\nJust some text.\n")

	ctx, ok := LoadProjectContext(tmp)
	if !ok {
		t.Fatal("expected context")
	}
	if ctx != "# Agent Instructions\n\nJust some text.\n" {
		t.Fatalf("expected verbatim raw content, got %q", ctx)
	}
}
