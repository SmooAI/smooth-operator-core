"""Conversation checkpointing — persist a turn's state so it can resume.

Phase-1 sibling of the reference engines' checkpointing. A :class:`CheckpointStore`
saves and loads the conversation (the non-system messages) keyed by a conversation
id, so a later turn — even in a new process — can pick up where the last left off.
:class:`InMemoryCheckpointStore` is the zero-dependency default; durable backends
implement the same protocol.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Protocol


@dataclass
class Checkpoint:
    """A saved conversation: its id and the non-system messages so far."""

    conversation_id: str
    messages: list[dict[str, Any]] = field(default_factory=list)


class CheckpointStore(Protocol):
    """Persists and restores conversations by id."""

    def save(self, checkpoint: Checkpoint) -> None: ...

    def load(self, conversation_id: str) -> Checkpoint | None: ...


class InMemoryCheckpointStore:
    """A process-local checkpoint store backed by a dict."""

    def __init__(self) -> None:
        self._store: dict[str, Checkpoint] = {}

    def save(self, checkpoint: Checkpoint) -> None:
        # Copy the messages so later mutation of the live list doesn't bleed in.
        self._store[checkpoint.conversation_id] = Checkpoint(
            conversation_id=checkpoint.conversation_id,
            messages=[dict(m) for m in checkpoint.messages],
        )

    def load(self, conversation_id: str) -> Checkpoint | None:
        return self._store.get(conversation_id)
