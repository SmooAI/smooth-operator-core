"""Knowledge reranking — reorder retrieved documents by relevance.

Phase-1 sibling of the reference engines' reranking. A :class:`Reranker` takes the
query and the retriever's candidate hits and returns them reordered (and possibly
trimmed) by a relevance signal. :class:`NoopReranker` is the default passthrough;
:class:`LexicalReranker` re-scores by query-term coverage normalized for document
length, so a concise on-topic doc outranks a long one with the same raw overlap.

The point is the seam: a cross-encoder or gateway reranker drops in behind the
same protocol. Used by the agent between retrieval and context injection.
"""

from __future__ import annotations

import math
import re
from typing import Protocol

from .knowledge import KnowledgeHit

_TOKEN_RE = re.compile(r"[a-z0-9]+")


def _tokens(text: str) -> list[str]:
    return _TOKEN_RE.findall(text.lower())


class Reranker(Protocol):
    """Reorders retrieved hits by relevance to the query."""

    def rerank(self, query: str, hits: list[KnowledgeHit]) -> list[KnowledgeHit]: ...


class NoopReranker:
    """Returns the hits unchanged — the zero-cost default."""

    def rerank(self, query: str, hits: list[KnowledgeHit]) -> list[KnowledgeHit]:
        return hits


class LexicalReranker:
    """Reorders by query-term coverage normalized by document length.

    Score = (distinct query terms present in the doc) / log2(2 + doc token count),
    so coverage is rewarded but long documents are penalized relative to concise
    ones with the same coverage. Stable for ties.
    """

    def rerank(self, query: str, hits: list[KnowledgeHit]) -> list[KnowledgeHit]:
        q_terms = set(_tokens(query))
        if not q_terms:
            return hits

        def score(hit: KnowledgeHit) -> float:
            doc_tokens = _tokens(hit.content)
            coverage = len(q_terms & set(doc_tokens))
            return coverage / math.log2(2 + len(doc_tokens))

        # Stable sort by descending score (Python's sort is stable, so ties keep
        # the retriever's order).
        return sorted(hits, key=score, reverse=True)
