"""Long-term memory — facts the agent carries across conversations.

Phase-1 sibling of the reference engines' memory. Distinct from checkpointing
(which persists a single conversation's messages): :class:`Memory` is a durable
pool of standalone facts the agent recalls into context on any turn, keyed by
relevance to the current message. :class:`InMemoryMemory` is the zero-dependency
default (lexical recall); a vector-backed memory drops in behind the protocol.

Recall behaviour here is a **cross-language contract**, not a local choice: the
Rust reference (``rust/smooth-operator-core/src/memory.rs``) and the C#, Go and
TypeScript siblings all produce the same block for the same memories. Pearl
th-ffaeae exists because they had drifted — three spellings of the header alone,
plus different scores, entry formats and top-k defaults, which made a shared
conformance scenario impossible to write.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from enum import Enum
from typing import Protocol

_TOKEN_RE = re.compile(r"[a-z0-9]+")

#: Memories auto-recalled per turn when the caller does not pass ``top_k``.
#: Matches ``MEMORY_TOP_K`` in every sibling core.
MEMORY_TOP_K = 5

#: Header that opens every auto-recall block, in every core.
RECALL_HEADER = "[Recalled memories]"

#: The verify-before-recommend note (rule D6), emitted only when at least one
#: recalled entry is time-sensitive (:meth:`MemoryType.needs_freshness_check`).
#:
#: A memory naming a function, file, or flag is a claim about the PAST, not a
#: fact about now — without this line the model happily recommends a symbol that
#: was deleted three releases ago.
RECALL_FRESHNESS_NOTE = (
    "Note: 'the memory says X exists' is not the same as 'X exists now'. "
    "Before recommending or acting on any function path, file, flag, or external "
    "pointer named below, verify it's current by reading the file or grepping the "
    "codebase. Project and Reference memories are time-sensitive; User and Feedback "
    "are durable."
)


def _tokens(text: str) -> list[str]:
    return _TOKEN_RE.findall(text.lower())


class MemoryType(str, Enum):
    """What kind of fact a memory holds. The name is rendered into the recall
    block, so these spellings are part of the cross-language contract."""

    SHORT_TERM = "ShortTerm"
    LONG_TERM = "LongTerm"
    ENTITY = "Entity"
    USER = "User"
    FEEDBACK = "Feedback"
    PROJECT = "Project"
    REFERENCE = "Reference"

    def needs_freshness_check(self) -> bool:
        """Whether this kind of memory can go stale.

        ``Project`` and ``Reference`` entries name things in a moving codebase or
        an external system; ``User`` and ``Feedback`` describe the person, which
        does not change when someone renames a file."""
        return self in (MemoryType.PROJECT, MemoryType.REFERENCE)


@dataclass(frozen=True)
class MemoryEntry:
    """One remembered fact.

    ``memory_type`` and ``relevance`` default so existing callers constructing
    ``MemoryEntry(text=...)`` keep working; both are rendered into the recall
    block, which is why they exist here at all."""

    text: str
    memory_type: MemoryType = field(default=MemoryType.LONG_TERM)
    relevance: float = 0.0


class Memory(Protocol):
    """A pool of remembered facts, recalled by relevance to a query."""

    def remember(self, text: str) -> None: ...

    def recall(self, query: str, top_k: int = MEMORY_TOP_K) -> list[MemoryEntry]: ...


def relevance_score(query: str, content: str) -> float:
    """Fraction of the QUERY's distinct tokens that appear in ``content``.

    Normalised to 0.0–1.0 deliberately: a raw overlap count is comparable neither
    between entries nor between languages, and it is rendered into the prompt.
    Punctuation is a token separator, not part of a token — the Rust reference
    used to split on whitespace alone, so a query ending "…my name?" never
    matched a memory containing "name" and the entry was silently not recalled."""
    query_tokens = set(_tokens(query))
    if not query_tokens:
        return 0.0
    content_tokens = set(_tokens(content))
    return len(query_tokens & content_tokens) / len(query_tokens)


def render_recall_block(entries: list[MemoryEntry]) -> str | None:
    """Render recalled entries as the context block injected into a turn.

    Byte-identical to the Rust reference's ``render_recall_block``. ``None`` for
    an empty list, so a caller injects nothing rather than a bare header telling
    the model it remembered nothing."""
    if not entries:
        return None
    lines = [RECALL_HEADER]
    if any(e.memory_type.needs_freshness_check() for e in entries):
        lines.append(RECALL_FRESHNESS_NOTE)
    lines.extend(f"- ({e.memory_type.value}, relevance={e.relevance:.2f}): {e.text}" for e in entries)
    return "\n".join(lines) + "\n"


class InMemoryMemory:
    """A process-local memory pool with lexical-overlap recall."""

    def __init__(self) -> None:
        self._entries: list[MemoryEntry] = []

    def remember(self, text: str, memory_type: MemoryType = MemoryType.LONG_TERM) -> None:
        text = text.strip()
        if text:
            self._entries.append(MemoryEntry(text=text, memory_type=memory_type))

    def recall(self, query: str, top_k: int = MEMORY_TOP_K) -> list[MemoryEntry]:
        if top_k <= 0:
            return []
        scored = [(relevance_score(query, e.text), e) for e in self._entries]
        # Only entries with some overlap, best first; stable for ties (insertion order).
        scored.sort(key=lambda pair: pair[0], reverse=True)
        return [
            MemoryEntry(text=e.text, memory_type=e.memory_type, relevance=score)
            for score, e in scored[:top_k]
            if score > 0
        ]
