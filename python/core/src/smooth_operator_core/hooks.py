"""Tool-call lifecycle hooks — the Python sibling of the Rust reference engine's
``ToolHook`` trait (``smooth-operator-core/src/tool.rs``) and the C# core's hook
seam.

A :class:`ToolHook` observes every tool call the agent dispatches: :meth:`pre_call`
runs before the tool (raise to BLOCK it) and :meth:`post_call` runs after, with a
**mutable** :class:`ToolResult`. That mutability is the point — ``post_call`` is a
redaction seam, not just an observation point: a hook may rewrite
``result.content`` (e.g. scrubbing a leaked secret) and the mutation is what
downstream consumers — the model, the conversation transcript, the stream — see.

Both methods default to a no-op, so a hook overrides only what it needs (mirroring
the Rust trait's default methods). This is a concrete base class rather than a
``Protocol`` precisely so subclasses inherit those defaults.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class ToolCall:
    """A tool call the model requested, as seen by a hook. Mirrors the Rust
    ``ToolCall`` (name + parsed arguments). ``arguments`` is the JSON-parsed dict
    the tool will receive."""

    name: str
    arguments: dict[str, Any] = field(default_factory=dict)


@dataclass
class ToolResult:
    """The result of executing a tool, handed to :meth:`ToolHook.post_call` as a
    **mutable** object. Rewriting :attr:`content` in a ``post_call`` hook is the
    redaction seam — the mutated content is what reaches the model and transcript.
    Mirrors the Rust ``ToolResult`` (content + ``is_error``)."""

    content: str
    is_error: bool = False


class ToolHook:
    """A hook that runs before and after every tool call. Subclass and override
    :meth:`pre_call` and/or :meth:`post_call` — both default to no-ops, so a hook
    implements only the phase it cares about.

    Parity with the Rust reference engine's ``ToolHook`` trait: ``pre_call`` may
    block a call by raising; ``post_call`` receives a mutable :class:`ToolResult`
    and may redact it in place.
    """

    async def pre_call(self, call: ToolCall) -> None:
        """Run before the tool executes. Raise to BLOCK the call — the exception
        message is surfaced to the model as the tool result and the tool never runs.
        The default is a no-op (allow)."""

    async def post_call(self, call: ToolCall, result: ToolResult) -> None:
        """Run after the tool executes, with a **mutable** ``result``. Rewrite
        ``result.content`` to redact what the model/transcript sees. A raise here is
        swallowed (the possibly-redacted result still reaches the caller — a broken
        hook must never crash the turn). The default is a no-op."""
