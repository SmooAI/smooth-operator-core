"""In-memory knowledge base for the Python smooth-operator core.

A minimal lexical-overlap retriever — the Phase-0 sibling of the Rust engine's
in-memory lexical knowledge. Documents are scored by token overlap with the
query; the top-k are returned. When nothing overlaps, the first k documents are
returned anyway so the agent still has context to ground (or honestly decline)
against — mirroring the reference engines' "always give the model something to
reason over" behavior.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Protocol

_TOKEN_RE = re.compile(r"[a-z0-9]+")


def _tokenize(text: str) -> list[str]:
    return _TOKEN_RE.findall(text.lower())


@dataclass(frozen=True)
class KnowledgeHit:
    """One retrieved document with its lexical-overlap score."""

    content: str
    source: str
    score: float


class Knowledge(Protocol):
    """A retriever: returns the most relevant documents for a query. Both the
    lexical :class:`InMemoryKnowledge` and the embedding-backed VectorKnowledge
    satisfy this, so the agent accepts either."""

    def query(self, query: str, top_k: int = 4) -> list[KnowledgeHit]: ...


@dataclass
class _Doc:
    content: str
    source: str


@dataclass
class InMemoryKnowledge:
    """A tiny lexical knowledge base. Not a vector store — Phase 0 parity only."""

    _docs: list[_Doc] = field(default_factory=list)

    def ingest(self, content: str, source: str) -> None:
        """Add a document to the knowledge base."""
        self._docs.append(_Doc(content=content, source=source))

    def query(self, query: str, top_k: int = 4) -> list[KnowledgeHit]:
        """Return up to ``top_k`` documents, ranked by token overlap with ``query``."""
        q_tokens = set(_tokenize(query))
        scored: list[tuple[int, _Doc]] = []
        for doc in self._docs:
            overlap = len(q_tokens & set(_tokenize(doc.content)))
            scored.append((overlap, doc))
        scored.sort(key=lambda pair: pair[0], reverse=True)

        hits = [
            KnowledgeHit(content=doc.content, source=doc.source, score=float(overlap))
            for overlap, doc in scored[:top_k]
            if overlap > 0
        ]
        if not hits:
            # No lexical overlap — still hand the model the first k docs so it can
            # ground or honestly decline, rather than retrieving nothing.
            hits = [KnowledgeHit(content=doc.content, source=doc.source, score=0.0) for doc in self._docs[:top_k]]
        return hits
