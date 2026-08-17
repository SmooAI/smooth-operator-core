"""Project context loader — AGENTS.md (or its fallbacks) plus resolved file references.

The Python port of the Rust reference engine's ``context.rs`` (pearl th-5002c4).

Smooth previously only read AGENTS.md. Many projects don't have one but DO have
CLAUDE.md or a SMOOTH.md or ``.smooth/CONTEXT.md``. User-level facts also belong in the
prompt, so we walk a preference order and **stack** user-level + project-level context.

Preference order (first hit per layer; layers stack):

- USER layer (read once, prepended):
  ``~/.smooth/CONTEXT.md`` → ``~/.smooth/AGENTS.md`` → ``~/.smooth/CLAUDE.md``

- PROJECT layer (walk up from ``working_dir``, first hit wins):
  ``<dir>/.smooth/CONTEXT.md`` → ``<dir>/SMOOTH.md`` → ``<dir>/AGENTS.md`` → ``<dir>/CLAUDE.md``

AGENTS.md / SMOOTH.md can carry file references in a ``## File References`` section::

    ## File References
    - [CLAUDE.md](CLAUDE.md) — full file
    - [Section name](CLAUDE.md#6-pearl-tracking) — specific section

Those are resolved against the file's directory and appended inline. The combined
string is what the host injects into the agent's system prompt — like the Rust
reference, this is a standalone loader, **not** a hook inside the agent loop.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

USER_CONTEXT_CANDIDATES = (".smooth/CONTEXT.md", ".smooth/AGENTS.md", ".smooth/CLAUDE.md")
PROJECT_CONTEXT_CANDIDATES = (".smooth/CONTEXT.md", "SMOOTH.md", "AGENTS.md", "CLAUDE.md")


@dataclass(frozen=True)
class FileRef:
    """A parsed file reference from the ``## File References`` section."""

    label: str
    """Display label from the markdown link text."""
    path: str
    """Relative file path (without fragment)."""
    fragment: str | None = None
    """Optional ``#fragment`` pointing to a heading."""
    description: str | None = None
    """Optional description after the ``` — ```."""


def load_project_context(working_dir: str | Path) -> str | None:
    """Load the combined project + user context, user-level prepended.

    File references in any AGENTS.md / SMOOTH.md are resolved inline. Returns ``None``
    only when NEITHER layer found anything — so a workspace with a bare CLAUDE.md and
    no user-level file still loads context.
    """
    user = _load_user_context()
    project = _load_layered_project_context(Path(working_dir))

    if user is None and project is None:
        return None
    if project is None:
        return f"## User context (~/.smooth)\n\n{user}"
    if user is None:
        return project
    return f"## User context (~/.smooth)\n\n{user}\n\n---\n\n{project}"


def _load_user_context() -> str | None:
    """First non-blank hit from the user-level preference list."""
    try:
        home = Path.home()
    except (RuntimeError, OSError):  # No home dir (minimal CI / broken containers).
        return None
    for candidate in USER_CONTEXT_CANDIDATES:
        raw = _read_or_none(home / candidate)
        if raw is not None and raw.strip():
            return raw
    return None


def _load_layered_project_context(working_dir: Path) -> str | None:
    """The project context file with its file references resolved."""
    context_path = _find_project_context_file(working_dir)
    if context_path is None:
        return None
    raw = _read_or_none(context_path)
    if raw is None:
        return None

    refs = parse_file_references(raw)
    if not refs:
        return raw

    resolved = _resolve_references(context_path.parent, refs)
    if not resolved:
        return raw

    parts = [raw, "\n---\n\n## Resolved File References\n\n"]
    for ref, content in resolved:
        parts.append(f"### {ref.label}\n" if ref.description is None else f"### {ref.label} — {ref.description}\n")
        parts.append("\n```\n")
        parts.append(content)
        if not content.endswith("\n"):
            parts.append("\n")
        parts.append("```\n\n")
    return "".join(parts)


def _find_project_context_file(start_dir: Path) -> Path | None:
    """Walk up from ``start_dir``; first candidate hit per directory wins."""
    directory = start_dir
    while True:
        for candidate in PROJECT_CONTEXT_CANDIDATES:
            path = directory / candidate
            if path.is_file():
                return path
        parent = directory.parent
        if parent == directory:
            return None
        directory = parent


def parse_file_references(content: str) -> list[FileRef]:
    """Parse the ``## File References`` section out of AGENTS.md content.

    Expects markdown list items like ``- [Label](path.md#fragment) — description``.
    """
    refs: list[FileRef] = []
    in_section = False

    for line in _split_lines(content):
        trimmed = line.strip()

        # Detect the file references section.
        if trimmed.startswith("## ") or trimmed.startswith("# "):
            in_section = "file reference" in trimmed.lower()
            continue
        if not in_section:
            continue

        ref = _parse_link_line(trimmed)
        if ref is not None:
            refs.append(ref)
    return refs


def _parse_link_line(line: str) -> FileRef | None:
    """Parse a single markdown list-item link line."""
    # Strip leading `- ` or `* `.
    if line.startswith("- ") or line.startswith("* "):
        rest = line[2:]
    else:
        return None

    # Match [label](target).
    open_bracket = rest.find("[")
    if open_bracket < 0:
        return None
    close_bracket = rest.find("]", open_bracket)
    if close_bracket < 0:
        return None
    label = rest[open_bracket + 1 : close_bracket]

    after = rest[close_bracket + 1 :]
    open_paren = after.find("(")
    if open_paren < 0:
        return None
    close_paren = after.find(")", open_paren)
    if close_paren < 0:
        return None
    target = after[open_paren + 1 : close_paren]

    # Split path and fragment.
    hash_pos = target.find("#")
    path = target if hash_pos < 0 else target[:hash_pos]
    fragment = None if hash_pos < 0 else target[hash_pos + 1 :]

    # Description after ` — ` / ` - ` / ` -- `.
    after_link = after[close_paren + 1 :]
    description = None
    for sep in (" — ", " - ", " -- "):
        if after_link.startswith(sep):
            candidate = after_link[len(sep) :].strip()
            if candidate:
                description = candidate
            break

    if not path and fragment is None:
        return None
    return FileRef(label=label, path=path, fragment=fragment, description=description)


def _resolve_references(base_dir: Path, refs: list[FileRef]) -> list[tuple[FileRef, str]]:
    """Resolve references against ``base_dir``, skipping unreadable or empty ones."""
    results: list[tuple[FileRef, str]] = []
    for ref in refs:
        raw = _read_or_none(base_dir / ref.path)
        if raw is None:
            continue  # Skip unreadable files.
        content = raw if ref.fragment is None else _extract_section(raw, ref.fragment)
        if content.strip():
            results.append((ref, content))
    return results


def _extract_section(content: str, fragment: str) -> str:
    """Extract a markdown section by heading fragment.

    The fragment is matched against GitHub-style heading anchors; the section runs
    until the next heading at the same or a higher level.
    """
    target = _heading_to_anchor(fragment)
    lines = _split_lines(content)
    start: int | None = None
    start_level = 0

    for i, line in enumerate(lines):
        heading = _parse_heading(line)
        if heading is None:
            continue
        level, text = heading
        anchor = _heading_to_anchor(text)
        if anchor == target or target in anchor or anchor in target:
            start = i
            start_level = level
            continue
        # Started capturing and hit a same-or-higher-level heading: stop.
        if start is not None and level <= start_level:
            return "\n".join(lines[start:i])

    return "\n".join(lines[start:]) if start is not None else ""


def _parse_heading(line: str) -> tuple[int, str] | None:
    """Parse a markdown heading line into ``(level, text)``."""
    trimmed = line.strip()
    if not trimmed.startswith("#"):
        return None
    level = len(trimmed) - len(trimmed.lstrip("#"))
    text = trimmed[level:].strip()
    if not text:
        return None
    return level, text


def _heading_to_anchor(text: str) -> str:
    """Convert heading text to a GitHub-style anchor."""
    out: list[str] = []
    for ch in text.lower():
        if ch.isalnum() or ch in "-_":
            out.append(ch)
        elif ch == " ":
            out.append("-")
        # Other characters are dropped.
    # Single non-overlapping pass, matching Rust's `str::replace`.
    return "".join(out).replace("--", "-")


def _read_or_none(path: Path) -> str | None:
    """``None`` instead of raising, for any unreadable path."""
    try:
        return path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError):
        return None


def _split_lines(s: str) -> list[str]:
    """Mirror Rust's ``str::lines``: split on ``\\n`` only, strip a trailing ``\\r``.

    ``str.splitlines`` would also break on ``\\v``, ``\\f`` and Unicode separators,
    which the Rust reference does not.
    """
    if not s:
        return []
    body = s[:-1] if s.endswith("\n") else s
    return [line[:-1] if line.endswith("\r") else line for line in body.split("\n")]
