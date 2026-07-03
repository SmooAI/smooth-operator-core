"""SEP host live-wire integration — the Python sibling of the Rust
``sep_extension_host.rs``.

Spawns a real extension subprocess (the dependency-free Python echo peer) through
the :class:`ExtensionHost`, and asserts the composition claim: an extension's tools
surface as dotted proxies, execute end-to-end over ``tool/execute``, and flow
through the SAME name-based ``enabled_tools`` retain a runner applies — so an
allow-list drops an extension tool exactly like it drops a built-in."""

from __future__ import annotations

import sys
from pathlib import Path

from smooth_operator_core.extension import (
    DefaultHostDelegate,
    ExtensionHost,
    HostInfo,
    WorkspaceInfo,
    discover,
)

ECHO_PEER = Path(__file__).parent / "sep" / "echo_peer.py"


def _write_echo_manifest(root: Path) -> None:
    ext_dir = root / "echo"
    ext_dir.mkdir(parents=True, exist_ok=True)
    (ext_dir / "extension.toml").write_text(
        f'name = "echo"\nversion = "0.1.0"\n[run]\ncommand = "{sys.executable}"\nargs = ["{ECHO_PEER}"]\n'
        "[capabilities]\ntools = true\n"
    )


async def _load_echo_host(directory: Path) -> ExtensionHost:
    discovered, failures = discover(directory, None)
    assert failures == [], f"manifest parse failures: {failures}"
    assert len(discovered) == 1
    host, load_failures = await ExtensionHost.load(
        discovered,
        HostInfo("test", "0"),
        WorkspaceInfo(str(directory), True),
        "widget",
        ["confirm"],
        DefaultHostDelegate(),
    )
    assert load_failures == [], f"extension load failures: {load_failures}"
    return host


def _survives_enabled_tools(host: ExtensionHost, enabled: set[str]) -> bool:
    """Register the host's tools, apply an enabled_tools retain (as the runner does),
    and report whether the ext tool survived."""
    tools = [t for t in host.tools() if t.name in enabled]
    return any(t.name == "echo.say" for t in tools)


async def test_extension_tool_reaches_host_and_honors_enabled_tools(tmp_path: Path) -> None:
    _write_echo_manifest(tmp_path)
    host = await _load_echo_host(tmp_path)
    try:
        assert host.names() == ["echo"]

        tools = host.tools()
        assert any(t.name == "echo.say" for t in tools), [t.name for t in tools]

        # enabled_tools that INCLUDES the ext tool keeps it; one that EXCLUDES it drops
        # it — exactly the filtering built-ins get.
        assert _survives_enabled_tools(host, {"echo.say"})
        assert not _survives_enabled_tools(host, {"some_builtin"})

        # The proxy executes end-to-end over tool/execute.
        say = next(t for t in host.tools() if t.name == "echo.say")
        assert await say.execute({"phrase": "hi there"}) == "hi there"
    finally:
        await host.shutdown_all()


async def test_untrusted_workspace_skips_project_extension(tmp_path: Path) -> None:
    # A project-scoped extension must not load in an untrusted workspace.
    from smooth_operator_core.extension.manifest import project_dir

    proj_ext = project_dir(tmp_path)
    _write_echo_manifest(proj_ext)
    discovered, _ = discover(None, proj_ext)
    assert len(discovered) == 1
    host, failures = await ExtensionHost.load(
        discovered,
        HostInfo("test", "0"),
        WorkspaceInfo(str(tmp_path), False),  # untrusted
        "widget",
        [],
        DefaultHostDelegate(),
    )
    try:
        assert host.is_empty()
        assert failures == []
    finally:
        await host.shutdown_all()
