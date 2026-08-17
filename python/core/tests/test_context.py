"""Ports the Rust reference engine's ``context.rs`` tests one-for-one."""

from __future__ import annotations

from pathlib import Path

from smooth_operator_core.context import (
    _extract_section,
    _find_project_context_file,
    _heading_to_anchor,
    _parse_link_line,
    load_project_context,
    parse_file_references,
)


def write(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def test_parse_simple_link() -> None:
    r = _parse_link_line("- [CLAUDE.md](CLAUDE.md) — Project overview")
    assert r is not None
    assert r.label == "CLAUDE.md"
    assert r.path == "CLAUDE.md"
    assert r.fragment is None
    assert r.description == "Project overview"


def test_parse_link_with_fragment() -> None:
    r = _parse_link_line("- [Pearl tracking](CLAUDE.md#6-pearl-tracking) — Pearl workflow")
    assert r is not None
    assert r.label == "Pearl tracking"
    assert r.path == "CLAUDE.md"
    assert r.fragment == "6-pearl-tracking"
    assert r.description == "Pearl workflow"


def test_parse_link_no_description() -> None:
    r = _parse_link_line("- [README](README.md)")
    assert r is not None
    assert r.label == "README"
    assert r.path == "README.md"
    assert r.fragment is None
    assert r.description is None


def test_parse_file_references_section() -> None:
    content = (
        "# Agent Instructions\n\nSome intro text.\n\n## File References\n\n"
        "- [CLAUDE.md](CLAUDE.md) — Full file\n"
        "- [Testing](CLAUDE.md#8-testing) — Testing reqs\n\n"
        "## Other Section\n\n"
        "- [not a ref](foo.md)\n"
    )
    refs = parse_file_references(content)
    assert len(refs) == 2
    assert refs[0].path == "CLAUDE.md"
    assert refs[0].fragment is None
    assert refs[1].path == "CLAUDE.md"
    assert refs[1].fragment == "8-testing"


def test_heading_to_anchor_basic() -> None:
    assert _heading_to_anchor("6. Pearl Tracking") == "6-pearl-tracking"
    assert _heading_to_anchor("Testing - MANDATORY") == "testing--mandatory"
    assert _heading_to_anchor("Simple Heading") == "simple-heading"


def test_extract_section_by_fragment() -> None:
    content = (
        "# Top\n\nIntro\n\n## Section A\n\nContent A\n\n## Section B\n\nContent B\n\n### Subsection\n\nSub content\n"
    )
    section = _extract_section(content, "section-a")
    assert "## Section A" in section
    assert "Content A" in section
    assert "Section B" not in section


def test_extract_section_to_eof() -> None:
    section = _extract_section("# Top\n\n## Last Section\n\nFinal content\n", "last-section")
    assert "## Last Section" in section
    assert "Final content" in section


def test_extract_section_not_found() -> None:
    assert _extract_section("# Top\n\n## Existing\n\nContent\n", "nonexistent") == ""


def test_load_from_temp_dir(tmp_path: Path) -> None:
    write(
        tmp_path / "CLAUDE.md",
        "# Project\n\nOverview\n\n## Testing\n\nAll tests must pass.\n\n## Deploy\n\nNever deploy locally.\n",
    )
    write(
        tmp_path / "AGENTS.md",
        "# Agent Instructions\n\n## File References\n\n- [Testing](CLAUDE.md#testing) — Test reqs\n\n## Rules\n\nBe helpful.\n",
    )

    ctx = load_project_context(tmp_path)
    assert ctx is not None
    assert "Agent Instructions" in ctx
    assert "Resolved File References" in ctx
    assert "All tests must pass" in ctx


def test_load_returns_nothing_referring_to_an_empty_dir(tmp_path: Path) -> None:
    # Mirrors the Rust test's caveat: the walk-up escapes to the filesystem root
    # and the user layer can't be mocked, so assert the practical guarantee —
    # nothing loaded refers to the (empty) temp dir.
    ctx = load_project_context(tmp_path)
    if ctx is not None:
        assert str(tmp_path) not in ctx


def test_load_prefers_smooth_context_over_claude_md(tmp_path: Path) -> None:
    write(tmp_path / ".smooth" / "CONTEXT.md", "# Smooth context\n\nthe winner")
    write(tmp_path / "CLAUDE.md", "# Claude.md\n\nshould lose")

    ctx = load_project_context(tmp_path)
    assert ctx is not None
    assert "the winner" in ctx
    assert "should lose" not in ctx


def test_load_falls_back_to_claude_md_when_no_agents(tmp_path: Path) -> None:
    write(tmp_path / "CLAUDE.md", "# CLAUDE.md\n\nfallback content")
    ctx = load_project_context(tmp_path)
    assert ctx is not None
    assert "fallback content" in ctx


def test_load_prefers_smooth_md_over_claude_md(tmp_path: Path) -> None:
    write(tmp_path / "SMOOTH.md", "# SMOOTH.md\n\nsmooth wins")
    write(tmp_path / "CLAUDE.md", "# CLAUDE.md\n\nclaude loses")

    ctx = load_project_context(tmp_path)
    assert ctx is not None
    assert "smooth wins" in ctx
    assert "claude loses" not in ctx


def test_find_project_context_walks_up(tmp_path: Path) -> None:
    nested = tmp_path / "a" / "b" / "c"
    nested.mkdir(parents=True)
    write(tmp_path / "CLAUDE.md", "# CLAUDE.md at root")

    found = _find_project_context_file(nested)
    assert found is not None
    assert found.name == "CLAUDE.md"


def test_load_without_file_references_returns_raw(tmp_path: Path) -> None:
    write(tmp_path / "AGENTS.md", "# Agent Instructions\n\nJust some text.\n")
    assert load_project_context(tmp_path) == "# Agent Instructions\n\nJust some text.\n"
