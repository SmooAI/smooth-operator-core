"""Conversation threads — carry message history across :meth:`SmoothAgent.run` calls.

Phase-2 sibling of the C# ``SmoothAgentThread`` (``dotnet/core``) and the reference
engine's persisted ``Conversation``. A :class:`SmoothAgentThread` is the in-memory
handle you hold per user conversation and pass to each run: the agent seeds the turn
from the thread's messages, runs, and appends this turn's user/assistant/tool messages
back to it, so the next turn has the full context. The system prompt is supplied
per-run from instructions/knowledge/memory and is *never* stored on the thread.

This complements checkpointing (:mod:`smooth_operator_core.checkpoint`): a checkpoint
*persists* a conversation to a store keyed by id; a thread is the live in-memory object
you pass between runs. The thread's :attr:`id` is the natural key to checkpoint under.
"""

from __future__ import annotations

import uuid
from typing import Any


class SmoothAgentThread:
    """A conversation thread: a stable id plus the ordered non-system messages so far.

    Construct fresh (a new id is generated) or pass an ``id`` to resume one
    (e.g. a key recovered from a checkpoint)::

        thread = SmoothAgentThread()                 # fresh conversation
        resumed = SmoothAgentThread(id="conv-42")    # resume by id
    """

    def __init__(self, id: str | None = None, messages: list[dict[str, Any]] | None = None) -> None:
        self.id = id if id else uuid.uuid4().hex
        # Ordered history, oldest first, never including a system message.
        self._messages: list[dict[str, Any]] = []
        if messages:
            self.extend(messages)

    @property
    def messages(self) -> list[dict[str, Any]]:
        """The accumulated history, oldest first (no system prompt)."""
        return self._messages

    def __len__(self) -> int:
        return len(self._messages)

    def add(self, message: dict[str, Any]) -> None:
        """Append one message, skipping any system message (rebuilt per-run)."""
        if message.get("role") == "system":
            return
        self._messages.append(message)

    def extend(self, messages: list[dict[str, Any]]) -> None:
        """Append several messages, skipping any system messages."""
        for m in messages:
            self.add(m)
