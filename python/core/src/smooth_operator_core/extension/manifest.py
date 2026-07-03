"""Extension manifests — ``extension.toml`` discovery, merge, and ``${env:VAR}``
expansion.

Python sibling of the Rust reference ``manifest.rs``. Same rules:

- An extension lives in a directory holding an ``extension.toml``.
- Global extensions: ``~/.smooth/extensions/<name>/extension.toml``.
- Project extensions: ``<workspace>/.smooth/extensions/<name>/extension.toml``.
- On a name collision the **project entry wins**.
- ``[run] env`` values support ``${env:VAR}`` expansion so secrets stay out of the
  manifest.
- A single malformed manifest is tolerated: it is collected as a failure and the
  rest still load.
"""

from __future__ import annotations

import os
import tomllib
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path
from typing import Any, Optional


class Scope(str, Enum):
    """Where a manifest was discovered. Project extensions only load in trusted
    workspaces; the host uses this to apply that policy."""

    GLOBAL = "global"
    PROJECT = "project"


@dataclass
class RunSpec:
    """How to launch the extension subprocess."""

    command: str
    args: list[str] = field(default_factory=list)
    env: dict[str, str] = field(default_factory=dict)

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> RunSpec:
        return cls(
            command=str(d["command"]),
            args=[str(a) for a in d.get("args", [])],
            env={str(k): str(v) for k, v in d.get("env", {}).items()},
        )


@dataclass
class Capabilities:
    """Capability declarations. ``events`` doubles as the host's dispatch filter —
    an extension only receives events it names here."""

    events: list[str] = field(default_factory=list)
    tools: bool = False
    commands: bool = False
    ui: bool = False
    exec: bool = False
    kv: bool = False
    bus: bool = False
    session: bool = False

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Capabilities:
        return cls(
            events=[str(e) for e in d.get("events", [])],
            tools=bool(d.get("tools", False)),
            commands=bool(d.get("commands", False)),
            ui=bool(d.get("ui", False)),
            exec=bool(d.get("exec", False)),
            kv=bool(d.get("kv", False)),
            bus=bool(d.get("bus", False)),
            session=bool(d.get("session", False)),
        )


@dataclass
class Resources:
    """Resource directories the extension contributes (skills, prompts, themes)."""

    skills: Optional[str] = None
    prompts: Optional[str] = None
    themes: Optional[str] = None

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Resources:
        return cls(skills=d.get("skills"), prompts=d.get("prompts"), themes=d.get("themes"))


@dataclass
class ExtensionManifest:
    """A parsed ``extension.toml``."""

    name: str
    version: str
    run: RunSpec
    protocol: int = 1
    capabilities: Capabilities = field(default_factory=Capabilities)
    resources: Resources = field(default_factory=Resources)
    hook_timeout_ms: Optional[int] = None
    disabled: bool = False

    @classmethod
    def parse(cls, toml_text: str) -> ExtensionManifest:
        """Parse a manifest from TOML text. Raises on malformed TOML or missing
        required fields (``name`` / ``version`` / ``[run] command``)."""
        try:
            raw = tomllib.loads(toml_text)
        except tomllib.TOMLDecodeError as exc:
            raise ValueError(f"parse extension.toml: {exc}") from exc
        if "run" not in raw or not isinstance(raw["run"], dict):
            raise ValueError("parse extension.toml: missing [run] table")
        return cls(
            name=str(raw["name"]),
            version=str(raw["version"]),
            run=RunSpec.from_dict(raw["run"]),
            protocol=int(raw.get("protocol", 1)),
            capabilities=Capabilities.from_dict(raw.get("capabilities", {})),
            resources=Resources.from_dict(raw.get("resources", {})),
            hook_timeout_ms=raw.get("hook_timeout_ms"),
            disabled=bool(raw.get("disabled", False)),
        )

    @classmethod
    def load_dir(cls, directory: Path) -> ExtensionManifest:
        """Load a manifest from ``<dir>/extension.toml``."""
        path = directory / "extension.toml"
        try:
            text = path.read_text(encoding="utf-8")
        except OSError as exc:
            raise ValueError(f"read {path}: {exc}") from exc
        return cls.parse(text)

    def resolved_env(self) -> dict[str, str]:
        """The ``[run] env`` map with ``${env:VAR}`` references expanded against the
        host's current environment. Unset variables expand to empty strings."""
        return {k: expand_env(v) for k, v in self.run.env.items()}


@dataclass
class DiscoveredExtension:
    """A discovered extension: its manifest, the directory it was found in (relative
    resources and ``args`` resolve against this root), and its scope."""

    manifest: ExtensionManifest
    root: Path
    scope: Scope


def default_global_dir() -> Optional[Path]:
    """Default global extensions directory: ``$SMOOTH_HOME/extensions`` if set, else
    ``~/.smooth/extensions``."""
    home = os.environ.get("SMOOTH_HOME")
    if home:
        return Path(home) / "extensions"
    try:
        return Path.home() / ".smooth" / "extensions"
    except RuntimeError:
        return None


def project_dir(workspace_root: Path) -> Path:
    """The project extensions directory for a workspace root."""
    return workspace_root / ".smooth" / "extensions"


def discover(
    global_dir: Optional[Path], project_dir_path: Optional[Path]
) -> tuple[list[DiscoveredExtension], list[tuple[str, str]]]:
    """Discover every extension under ``global_dir`` and ``project_dir_path``, merging
    by name with **project winning**. Either directory may be ``None`` or missing
    (treated as empty). Returns the chosen extensions plus a list of
    ``(name_or_dir, error)`` for manifests that failed to parse — a single bad
    manifest never aborts discovery."""
    failures: list[tuple[str, str]] = []
    by_name: dict[str, DiscoveredExtension] = {}

    # Global first, then project, so project overwrites on name collision.
    for directory, scope in ((global_dir, Scope.GLOBAL), (project_dir_path, Scope.PROJECT)):
        if directory is None:
            continue
        for found in _scan_dir(directory, scope, failures):
            by_name[found.manifest.name] = found

    # Stable (name-sorted) order so load-order-dependent hook chaining is deterministic.
    chosen = sorted(by_name.values(), key=lambda e: e.manifest.name)
    return chosen, failures


def _scan_dir(directory: Path, scope: Scope, failures: list[tuple[str, str]]) -> list[DiscoveredExtension]:
    """Scan a single extensions directory: each immediate subdirectory holding an
    ``extension.toml`` is one extension."""
    out: list[DiscoveredExtension] = []
    try:
        entries = sorted(directory.iterdir())
    except OSError:
        # Missing dir is not an error — just no extensions from this scope.
        return out
    for root in entries:
        if not root.is_dir() or not (root / "extension.toml").is_file():
            continue
        try:
            manifest = ExtensionManifest.load_dir(root)
        except ValueError as exc:
            failures.append((str(root), str(exc)))
            continue
        out.append(DiscoveredExtension(manifest=manifest, root=root, scope=scope))
    return out


def expand_env(value: str) -> str:
    """Expand ``${env:VAR}`` references using the host's current environment. Unset
    variables expand to empty strings. An unterminated ``${env:`` is left verbatim."""
    out: list[str] = []
    rest = value
    while True:
        idx = rest.find("${env:")
        if idx == -1:
            out.append(rest)
            break
        out.append(rest[:idx])
        after = rest[idx + 6 :]
        end = after.find("}")
        if end == -1:
            out.append(rest[idx:])
            break
        var = after[:end]
        out.append(os.environ.get(var, ""))
        rest = after[end + 1 :]
    return "".join(out)
