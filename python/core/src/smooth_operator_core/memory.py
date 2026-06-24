"""Long-term memory — facts the agent carries across conversations.

Phase-1 sibling of the reference engines' memory. Distinct from checkpointing
(which persists a single conversation's messages): :class:`Memory` is a durable
pool of standalone facts the agent recalls into context on any turn, keyed by
relevance to the current message. :class:`InMemoryMemory` is the zero-dependency
default (lexical recall); a vector-backed memory drops in behind the protocol.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Protocol

_TOKEN_RE = re.compile(r"[a-z0-9]+")


def _tokens(text: str) -> list[str]:
    return _TOKEN_RE.findall(text.lower())


@dataclass(frozen=True)
class MemoryEntry:
    """One remembered fact."""

    text: str


class Memory(Protocol):
    """A pool of remembered facts, recalled by relevance to a query."""

    def remember(self, text: str) -> None: ...

    def recall(self, query: str, top_k: int = 4) -> list[MemoryEntry]: ...


class InMemoryMemory:
    """A process-local memory pool with lexical-overlap recall."""

    def __init__(self) -> None:
        self._entries: list[MemoryEntry] = []

    def remember(self, text: str) -> None:
        text = text.strip()
        if text:
            self._entries.append(MemoryEntry(text=text))

    def recall(self, query: str, top_k: int = 4) -> list[MemoryEntry]:
        if top_k <= 0:
            return []
        q_terms = set(_tokens(query))
        scored = [(len(q_terms & set(_tokens(e.text))), e) for e in self._entries]
        # Only return entries with some overlap, best first; stable for ties.
        scored.sort(key=lambda pair: pair[0], reverse=True)
        return [e for overlap, e in scored[:top_k] if overlap > 0]
