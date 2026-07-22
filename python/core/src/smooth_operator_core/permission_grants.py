"""Persistent permission grants — ``wonk-allow.toml`` (pearl th-22bfc1).

The Python sibling of the Rust reference engine's ``permission_grants.rs``. The
:class:`~.permission.PermissionHook` gate closes on an ``Ask`` verdict by
prompting a human. Without persistence that prompt is *approve-once*: the same
command re-asks on every run. This module ports smooth's ``wonk-allow.toml``
allow-list so a human's "approve always" answer is remembered — a stored grant
that matches a later ``Ask`` auto-approves it **without prompting**.

Two TOML files are stacked at load time (project wins on collision):

- ``~/.smooth/wonk-allow.toml`` — the user's personal grants.
- ``<repo>/.smooth/wonk-allow.toml`` — project-scoped grants.

Schema (v1), interoperable with the Rust engine's file::

    schema_version = 1

    [network]
    allow_hosts = ["api.openai.com", "*.openai.com"]

    [tools]
    allow = ["web_search", "vendor.file_write"]

    [bash]
    allow_patterns = ["cargo ", "pnpm "]

- ``network.allow_hosts`` — exact host or ``*.suffix`` glob (case-insensitive).
- ``tools.allow`` — exact tool name.
- ``bash.allow_patterns`` — a command *prefix*; the trailing space in ``"cargo "``
  is significant (stops it matching ``cargonaut``).

There is no deny section: a stored grant can only upgrade an ``Ask``, **never**
waive a ``Deny`` circuit-breaker (see :mod:`.permission`).

Robustness:

- Missing file → empty store (first run needs no ``touch``).
- Malformed file → error surfaced (not silently ignored).
- Writes are atomic (tempfile + rename): a crash mid-save leaves the previous
  file intact.
"""

from __future__ import annotations

import os
import tomllib
from dataclasses import dataclass, field
from pathlib import Path
from threading import Lock
from typing import Union


# ---------------------------------------------------------------------------
# Grant queries — the three grantable ``Ask`` shapes (a ``Deny`` is never grantable)
# ---------------------------------------------------------------------------
@dataclass(frozen=True)
class NetworkGrant:
    """A network host (or ``*.suffix`` glob)."""

    host: str


@dataclass(frozen=True)
class ToolGrant:
    """An exact tool name (write / unknown tool)."""

    name: str


@dataclass(frozen=True)
class BashGrant:
    """A bash command prefix, e.g. ``"npm "``."""

    prefix: str


#: The kind of resource a grant covers. Mirrors the Rust ``GrantQuery`` enum.
GrantQuery = Union[NetworkGrant, ToolGrant, BashGrant]


def host_matches_glob(host: str, pattern: str) -> bool:
    """Glob match for a single host pattern (case-insensitive):

    - exact host: ``api.example.com`` matches only that.
    - ``*.example.com`` / ``.example.com``: any subdomain **and** the bare apex.
    - a bare suffix (``example.com``) matches only itself (no substring match, so
      ``evil-example.com`` never slips past ``example.com``).
    """
    h = host.lower()
    p = pattern.lower()
    if h == p:
        return True
    if p.startswith("*."):
        suffix = p[2:]
        return h.endswith(f".{suffix}") or h == suffix
    if p.startswith("."):
        suffix = p[1:]
        return h.endswith(f".{suffix}") or h == suffix
    return False


@dataclass
class PermissionGrants:
    """In-memory snapshot of ``wonk-allow.toml``."""

    #: Always 1. Reserved for forward-compatible migrations.
    schema_version: int = 0
    allow_hosts: set[str] = field(default_factory=set)
    allow_tools: set[str] = field(default_factory=set)
    allow_patterns: set[str] = field(default_factory=set)

    @staticmethod
    def new() -> "PermissionGrants":
        """New grants pinned at the current schema version."""
        return PermissionGrants(schema_version=1)

    # ── matching ───────────────────────────────────────────────────
    def matches_host(self, host: str) -> bool:
        """True if ``host`` is covered by the ``[network]`` allow-list."""
        lower = host.lower()
        return any(host_matches_glob(lower, pat) for pat in self.allow_hosts)

    def matches_tool(self, tool_name: str) -> bool:
        """True if ``tool_name`` is in the ``[tools]`` allow-list (exact match)."""
        return tool_name in self.allow_tools

    def matches_bash(self, command: str) -> bool:
        """True if ``command`` starts with any ``[bash]`` allow prefix."""
        lower = command.lower()
        return any(lower.startswith(p.lower()) for p in self.allow_patterns)

    def contains(self, query: GrantQuery) -> bool:
        """True if ``query``'s entry is already covered by the store."""
        if isinstance(query, NetworkGrant):
            return self.matches_host(query.host)
        if isinstance(query, ToolGrant):
            return self.matches_tool(query.name)
        return self.matches_bash(query.prefix)

    # ── mutation ───────────────────────────────────────────────────
    def add(self, query: GrantQuery) -> None:
        """Add a grant. Idempotent."""
        if isinstance(query, NetworkGrant):
            self.allow_hosts.add(query.host)
        elif isinstance(query, ToolGrant):
            self.allow_tools.add(query.name)
        else:
            self.allow_patterns.add(query.prefix)

    def merge_with(self, other: "PermissionGrants") -> None:
        """Union ``other`` into ``self``."""
        self.schema_version = max(self.schema_version, other.schema_version)
        self.allow_hosts |= other.allow_hosts
        self.allow_tools |= other.allow_tools
        self.allow_patterns |= other.allow_patterns

    def clone(self) -> "PermissionGrants":
        return PermissionGrants(
            schema_version=self.schema_version,
            allow_hosts=set(self.allow_hosts),
            allow_tools=set(self.allow_tools),
            allow_patterns=set(self.allow_patterns),
        )

    # ── serialization ──────────────────────────────────────────────
    @staticmethod
    def parse(toml_text: str) -> "PermissionGrants":
        """Parse from a TOML string. Missing sections default to empty."""
        data = tomllib.loads(toml_text)
        return PermissionGrants(
            schema_version=int(data.get("schema_version", 0) or 0),
            allow_hosts=set(_str_list(data.get("network", {}).get("allow_hosts", []))),
            allow_tools=set(_str_list(data.get("tools", {}).get("allow", []))),
            allow_patterns=set(_str_list(data.get("bash", {}).get("allow_patterns", []))),
        )

    def to_toml_string(self) -> str:
        """Serialize to TOML. Empty sections are omitted (matching the Rust engine)."""
        lines = [f"schema_version = {self.schema_version or 1}"]
        if self.allow_hosts:
            lines += ["", "[network]", f"allow_hosts = {_toml_array(self.allow_hosts)}"]
        if self.allow_tools:
            lines += ["", "[tools]", f"allow = {_toml_array(self.allow_tools)}"]
        if self.allow_patterns:
            lines += ["", "[bash]", f"allow_patterns = {_toml_array(self.allow_patterns)}"]
        return "\n".join(lines) + "\n"

    @staticmethod
    def load_from_path(path: Path) -> "PermissionGrants":
        """Load from ``path``. A missing file yields an empty (v1) store — **not**
        an error. A malformed file surfaces the parse error."""
        try:
            text = path.read_text()
        except FileNotFoundError:
            return PermissionGrants.new()
        try:
            return PermissionGrants.parse(text)
        except Exception as e:  # noqa: BLE001 — re-raise with context (fail loud, not silent)
            raise ValueError(f"malformed wonk-allow.toml at {path}: {e}") from e

    @staticmethod
    def load_layered(user: Path | None, project: Path | None) -> "PermissionGrants":
        """Load user + project files and merge them (**project wins** on collision —
        though for pure union allow-lists "wins" only affects ``schema_version``).
        Either path missing is fine; a malformed file present is an error."""
        merged = PermissionGrants.new()
        if user is not None:
            merged.merge_with(PermissionGrants.load_from_path(user))
        if project is not None:
            merged.merge_with(PermissionGrants.load_from_path(project))
        return merged

    def save_to_path(self, path: Path) -> None:
        """Atomically write to ``path`` (tempfile + rename), creating parent dirs."""
        path.parent.mkdir(parents=True, exist_ok=True)
        tmp = path.with_suffix(path.suffix + ".tmp")
        tmp.write_text(self.to_toml_string())
        os.replace(tmp, path)


def _str_list(value: object) -> list[str]:
    """Coerce a parsed TOML value into a list of strings (defensive against junk)."""
    if isinstance(value, list):
        return [v for v in value if isinstance(v, str)]
    return []


def _toml_escape(s: str) -> str:
    return s.replace("\\", "\\\\").replace('"', '\\"')


def _toml_array(values: set[str]) -> str:
    """A TOML inline array of quoted strings, sorted for deterministic output."""
    return "[" + ", ".join(f'"{_toml_escape(v)}"' for v in sorted(values)) + "]"


def user_grants_path() -> Path | None:
    """The user-scope grants file: ``~/.smooth/wonk-allow.toml``. ``None`` when
    there is no home dir (minimal CI / broken containers)."""
    try:
        home = Path.home()
    except (RuntimeError, OSError):
        return None
    return home / ".smooth" / "wonk-allow.toml"


def project_grants_path(workspace: Path) -> Path:
    """The project-scope grants file: ``<workspace>/.smooth/wonk-allow.toml``."""
    return workspace / ".smooth" / "wonk-allow.toml"


def append_grant(path: Path, query: GrantQuery) -> None:
    """Load the grant at ``path``, add ``query``, and atomically save. Creates the
    file if absent. Idempotent for a query that's already stored."""
    grants = PermissionGrants.load_from_path(path)
    if grants.schema_version == 0:
        grants.schema_version = 1
    grants.add(query)
    grants.save_to_path(path)


class SharedGrants:
    """Thread-safe, cheaply-shared handle to the live merged grants. Reads take a
    snapshot; approve-always merges the freshly-persisted grant back in. Mirrors
    the Rust ``SharedGrants`` (``Arc<RwLock<PermissionGrants>>``)."""

    def __init__(self, grants: PermissionGrants | None = None) -> None:
        self._inner = grants if grants is not None else PermissionGrants.new()
        self._lock = Lock()

    def snapshot(self) -> PermissionGrants:
        """A cloned-out snapshot for matching (mutating it does not touch the store)."""
        with self._lock:
            return self._inner.clone()

    def merge_in(self, other: PermissionGrants) -> None:
        """Union ``other`` into the live grants."""
        with self._lock:
            self._inner.merge_with(other)
