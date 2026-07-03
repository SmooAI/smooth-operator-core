"""Unit tests for extension.toml discovery, merge, and ${env:VAR} expansion."""

from __future__ import annotations

from pathlib import Path

from smooth_operator_core.extension.manifest import (
    ExtensionManifest,
    Scope,
    discover,
    expand_env,
)

MINIMAL = """
name = "echo"
version = "0.1.0"
[run]
command = "node"
args = ["echo.mjs"]
"""


def test_parses_minimal_manifest_with_defaults() -> None:
    m = ExtensionManifest.parse(MINIMAL)
    assert m.name == "echo"
    assert m.protocol == 1
    assert m.run.command == "node"
    assert m.run.args == ["echo.mjs"]
    assert not m.disabled
    assert m.capabilities.events == []


def test_parses_full_manifest() -> None:
    text = """
name = "gate"
version = "2.0.0"
protocol = 1
hook_timeout_ms = 3000
[run]
command = "python3"
args = ["-m", "gate"]
env = { TOKEN = "${env:GATE_TOKEN}", STATIC = "x" }
[capabilities]
events = ["turn_start", "tool_call"]
tools = true
ui = true
[resources]
skills = "skills"
"""
    m = ExtensionManifest.parse(text)
    assert m.hook_timeout_ms == 3000
    assert m.capabilities.tools and m.capabilities.ui and not m.capabilities.exec
    assert m.capabilities.events == ["turn_start", "tool_call"]
    assert m.resources.skills == "skills"


def test_malformed_manifest_errors() -> None:
    import pytest

    with pytest.raises(ValueError):
        ExtensionManifest.parse("not toml : : :")
    with pytest.raises((ValueError, KeyError)):
        ExtensionManifest.parse('name = "x"\nversion = "1"\n')  # missing [run]


def test_resolved_env_expands_env_refs(monkeypatch) -> None:
    monkeypatch.setenv("SEP_TEST_TOKEN", "secret123")
    monkeypatch.delenv("SEP_TEST_UNSET_XYZ", raising=False)
    text = """
name = "e"
version = "1"
[run]
command = "c"
env = { A = "pre-${env:SEP_TEST_TOKEN}-post", B = "${env:SEP_TEST_UNSET_XYZ}" }
"""
    env = ExtensionManifest.parse(text).resolved_env()
    assert env["A"] == "pre-secret123-post"
    assert env["B"] == ""


def test_expand_env_handles_unterminated_ref() -> None:
    assert expand_env("a${env:FOO") == "a${env:FOO"
    assert expand_env("plain") == "plain"


def _write_ext(directory: Path, name: str, body: str) -> None:
    ext_dir = directory / name
    ext_dir.mkdir(parents=True, exist_ok=True)
    (ext_dir / "extension.toml").write_text(body)


def test_discover_merges_project_over_global(tmp_path: Path) -> None:
    global_dir = tmp_path / "global"
    project = tmp_path / "project"
    _write_ext(global_dir, "echo", 'name="echo"\nversion="1.0.0"\n[run]\ncommand="g"\n')
    _write_ext(global_dir, "only_global", 'name="only_global"\nversion="1"\n[run]\ncommand="g"\n')
    _write_ext(project, "echo", 'name="echo"\nversion="2.0.0"\n[run]\ncommand="p"\n')
    _write_ext(project, "only_project", 'name="only_project"\nversion="1"\n[run]\ncommand="p"\n')

    found, failures = discover(global_dir, project)
    assert failures == []
    assert len(found) == 3
    echo = next(e for e in found if e.manifest.name == "echo")
    assert echo.manifest.version == "2.0.0"  # project won
    assert echo.scope == Scope.PROJECT
    assert any(e.manifest.name == "only_global" and e.scope == Scope.GLOBAL for e in found)
    assert any(e.manifest.name == "only_project" for e in found)


def test_discover_tolerates_one_broken_manifest(tmp_path: Path) -> None:
    global_dir = tmp_path / "g"
    _write_ext(global_dir, "good", 'name="good"\nversion="1"\n[run]\ncommand="c"\n')
    _write_ext(global_dir, "bad", "this is not = = valid toml\n[[[")

    found, failures = discover(global_dir, None)
    assert len(found) == 1
    assert found[0].manifest.name == "good"
    assert len(failures) == 1
    assert "bad" in failures[0][0]


def test_discover_missing_dirs_is_empty_not_error() -> None:
    found, failures = discover(Path("/no/such/global"), Path("/no/such/project"))
    assert found == []
    assert failures == []
