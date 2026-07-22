"""Persistent grant store tests — ported from the Rust ``permission_grants.rs``
test module."""

from __future__ import annotations

import pytest

from smooth_operator_core.permission_grants import (
    BashGrant,
    NetworkGrant,
    PermissionGrants,
    SharedGrants,
    ToolGrant,
    append_grant,
    project_grants_path,
    user_grants_path,
)


def test_new_pins_schema_version_one():
    assert PermissionGrants().schema_version == 0
    assert PermissionGrants.new().schema_version == 1


def test_host_exact_and_wildcard():
    g = PermissionGrants.new()
    g.add(NetworkGrant("api.example.com"))
    assert g.matches_host("api.example.com")
    assert g.matches_host("API.EXAMPLE.COM")
    assert not g.matches_host("other.example.com")

    w = PermissionGrants.new()
    w.add(NetworkGrant("*.example.com"))
    assert w.matches_host("api.example.com")
    assert w.matches_host("example.com")  # bare apex
    assert not w.matches_host("evil-example.com")


def test_bare_host_requires_exact_match():
    g = PermissionGrants.new()
    g.add(NetworkGrant("example.com"))
    assert g.matches_host("example.com")
    assert not g.matches_host("api.example.com")
    assert not g.matches_host("evil-example.com")


def test_tool_exact_only():
    g = PermissionGrants.new()
    g.add(ToolGrant("web_search"))
    assert g.matches_tool("web_search")
    assert not g.matches_tool("web_search_v2")


def test_bash_prefix_with_trailing_space_guard():
    g = PermissionGrants.new()
    g.add(BashGrant("cargo "))
    assert g.matches_bash("cargo test")
    assert g.matches_bash("CARGO BUILD")
    assert not g.matches_bash("cargonaut")


def test_contains_matches_add():
    g = PermissionGrants.new()
    q = BashGrant("npm ")
    assert not g.contains(q)
    g.add(q)
    assert g.contains(q)


def test_merge_unions():
    a = PermissionGrants.new()
    a.add(NetworkGrant("a.example.com"))
    b = PermissionGrants.new()
    b.add(ToolGrant("t"))
    b.add(BashGrant("pnpm "))
    a.merge_with(b)
    assert a.matches_host("a.example.com")
    assert a.matches_tool("t")
    assert a.matches_bash("pnpm i")


def test_save_load_round_trip(tmp_path):
    path = tmp_path / "wonk-allow.toml"
    g = PermissionGrants.new()
    g.add(NetworkGrant("*.openai.com"))
    g.add(ToolGrant("web_search"))
    g.add(BashGrant("cargo "))
    g.save_to_path(path)
    loaded = PermissionGrants.load_from_path(path)
    assert loaded.schema_version == g.schema_version
    assert loaded.allow_hosts == g.allow_hosts
    assert loaded.allow_tools == g.allow_tools
    assert loaded.allow_patterns == g.allow_patterns


def test_load_missing_is_empty_not_error(tmp_path):
    g = PermissionGrants.load_from_path(tmp_path / "nope.toml")
    assert g.schema_version == 1
    assert not g.allow_hosts


def test_load_malformed_surfaces_error(tmp_path):
    path = tmp_path / "wonk-allow.toml"
    path.write_text("this is [not valid = toml")
    with pytest.raises(ValueError, match="malformed wonk-allow.toml"):
        PermissionGrants.load_from_path(path)


def test_save_is_atomic_and_creates_dirs(tmp_path):
    path = tmp_path / "nested" / "dir" / "wonk-allow.toml"
    g = PermissionGrants.new()
    g.add(NetworkGrant("a.example.com"))
    g.save_to_path(path)
    assert path.exists()
    assert not path.with_suffix(path.suffix + ".tmp").exists()


def test_append_grant_creates_then_extends_idempotently(tmp_path):
    path = tmp_path / "wonk-allow.toml"
    append_grant(path, BashGrant("npm "))
    append_grant(path, BashGrant("npm "))  # dup
    append_grant(path, NetworkGrant("api.example.com"))
    g = PermissionGrants.load_from_path(path)
    assert len(g.allow_patterns) == 1
    assert g.matches_bash("npm install left-pad")
    assert g.matches_host("api.example.com")


def test_load_layered_project_wins_schema_but_unions_entries(tmp_path):
    user = tmp_path / "user.toml"
    project = tmp_path / "project.toml"
    u = PermissionGrants.new()
    u.add(BashGrant("cargo "))
    u.save_to_path(user)
    p = PermissionGrants.new()
    p.add(BashGrant("pnpm "))
    p.add(ToolGrant("web_search"))
    p.save_to_path(project)

    merged = PermissionGrants.load_layered(user, project)
    assert merged.matches_bash("cargo test")
    assert merged.matches_bash("pnpm i")
    assert merged.matches_tool("web_search")


def test_load_layered_missing_files_yield_empty(tmp_path):
    merged = PermissionGrants.load_layered(tmp_path / "u.toml", tmp_path / "p.toml")
    assert not merged.allow_hosts
    assert not merged.allow_patterns


def test_shared_snapshot_is_isolated_and_merge_visible():
    shared = SharedGrants(PermissionGrants.new())
    more = PermissionGrants.new()
    more.add(NetworkGrant("b.example.com"))
    shared.merge_in(more)
    assert shared.snapshot().matches_host("b.example.com")
    snap = shared.snapshot()
    snap.add(NetworkGrant("c.example.com"))
    assert not shared.snapshot().matches_host("c.example.com")


def test_path_helpers():
    p = user_grants_path()
    if p is not None:
        assert str(p).endswith(".smooth/wonk-allow.toml")
    from pathlib import Path

    assert project_grants_path(Path("/tmp/x")) == Path("/tmp/x/.smooth/wonk-allow.toml")
