"""Unit tests for knowledge reranking."""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from smooth_operator_core import (
    AgentOptions,
    InMemoryKnowledge,
    KnowledgeHit,
    LexicalReranker,
    NoopReranker,
    SmoothAgent,
)


def _hit(content: str, source: str = "s") -> KnowledgeHit:
    return KnowledgeHit(content=content, source=source, score=0.0)


def test_noop_reranker_passthrough():
    hits = [_hit("a"), _hit("b")]
    assert NoopReranker().rerank("q", hits) == hits


def test_lexical_reranker_prefers_concise_over_long_same_coverage():
    concise = _hit("return policy")
    filler = " ".join(["filler"] * 60)
    verbose = _hit(f"return {filler} policy")
    # Same coverage (return + policy) but the long doc should rank lower.
    out = LexicalReranker().rerank("return policy", [verbose, concise])
    assert out[0] is concise
    assert out[1] is verbose


def test_lexical_reranker_prefers_higher_coverage():
    covers_two = _hit("return and policy details")
    covers_one = _hit("return shipping details")
    out = LexicalReranker().rerank("return policy", [covers_one, covers_two])
    assert out[0] is covers_two


def test_empty_query_is_passthrough():
    hits = [_hit("a"), _hit("b")]
    assert LexicalReranker().rerank("", hits) == hits


# ── agent integration: reranker changes which doc gets injected ──────────────
def _resp(content):
    return SimpleNamespace(
        choices=[SimpleNamespace(message=SimpleNamespace(content=content, tool_calls=None))], usage=None
    )


class _FakeCompletions:
    def __init__(self, scripted):
        self._scripted = list(scripted)
        self.calls: list[list] = []

    async def create(self, **kwargs):
        self.calls.append(kwargs["messages"])
        return self._scripted.pop(0)


class FakeClient:
    def __init__(self, scripted):
        self.chat = SimpleNamespace(completions=_FakeCompletions(scripted))


@pytest.mark.asyncio
async def test_reranker_applied_between_retrieval_and_injection():
    kb = InMemoryKnowledge()
    # A long doc ingested FIRST (so the lexical retriever, which ties on overlap,
    # returns it before the concise one); the reranker should promote the concise.
    filler = " ".join(["filler"] * 60)
    kb.ingest(f"return {filler} policy", "long.md")
    kb.ingest("return policy", "short.md")

    client = FakeClient([_resp("ok")])
    agent = SmoothAgent(
        client,
        AgentOptions(
            instructions="support",
            knowledge=kb,
            knowledge_top_k=1,
            knowledge_candidate_k=2,
            reranker=LexicalReranker(),
        ),
    )
    await agent.run("return policy")
    system = client.chat.completions.calls[0][0]["content"]
    # The reranked top-1 is the concise doc, so it (not the long one) is injected.
    assert "[short.md]" in system
    assert "[long.md]" not in system
