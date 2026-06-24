"""Vector knowledge — embedding-backed semantic retrieval.

Phase-1 sibling of the reference engines' vector store. :class:`VectorKnowledge`
embeds documents and queries and retrieves by cosine similarity, satisfying the
same :class:`Knowledge` protocol as the lexical retriever (so the agent accepts
either). The :class:`Embedder` is pluggable: the default :class:`HashEmbedder` is
deterministic and offline (feature-hashed bag-of-words) — good for tests and a
zero-dependency default — while a gateway embedder (e.g. an ``/embeddings`` call)
drops in behind the same protocol for true semantics.
"""

from __future__ import annotations

import math
import re
from typing import Protocol

from .knowledge import KnowledgeHit

_TOKEN_RE = re.compile(r"[a-z0-9]+")


def _tokens(text: str) -> list[str]:
    return _TOKEN_RE.findall(text.lower())


class Embedder(Protocol):
    """Turns text into a fixed-length vector."""

    def embed(self, text: str) -> list[float]: ...


class HashEmbedder:
    """Deterministic, offline feature-hashing embedder.

    Hashes each token into one of ``dim`` buckets (signed) and L2-normalizes the
    result. No learned semantics, but a real vector with cosine geometry — docs
    sharing tokens land near each other — and stable across processes.
    """

    def __init__(self, dim: int = 256) -> None:
        if dim <= 0:
            raise ValueError("dim must be positive")
        self.dim = dim

    def embed(self, text: str) -> list[float]:
        vec = [0.0] * self.dim
        for tok in _tokens(text):
            h = hash_token(tok)
            bucket = h % self.dim
            sign = 1.0 if (h >> 31) & 1 == 0 else -1.0
            vec[bucket] += sign
        norm = math.sqrt(sum(v * v for v in vec))
        if norm > 0:
            vec = [v / norm for v in vec]
        return vec


def hash_token(token: str) -> int:
    """A small deterministic non-cryptographic hash (FNV-1a, 32-bit)."""
    h = 0x811C9DC5
    for ch in token.encode("utf-8"):
        h ^= ch
        h = (h * 0x01000193) & 0xFFFFFFFF
    return h


def _cosine(a: list[float], b: list[float]) -> float:
    return sum(x * y for x, y in zip(a, b))  # both are L2-normalized


class VectorKnowledge:
    """An embedding-backed knowledge store with cosine-similarity retrieval."""

    def __init__(self, embedder: Embedder | None = None) -> None:
        self._embedder: Embedder = embedder or HashEmbedder()
        self._docs: list[tuple[list[float], str, str]] = []  # (embedding, content, source)

    def ingest(self, content: str, source: str) -> None:
        self._docs.append((self._embedder.embed(content), content, source))

    def query(self, query: str, top_k: int = 4) -> list[KnowledgeHit]:
        if top_k <= 0 or not self._docs:
            return []
        q = self._embedder.embed(query)
        scored = [(_cosine(q, emb), content, source) for emb, content, source in self._docs]
        scored.sort(key=lambda t: t[0], reverse=True)
        return [
            KnowledgeHit(content=content, source=source, score=score)
            for score, content, source in scored[:top_k]
            if score > 0.0
        ]
