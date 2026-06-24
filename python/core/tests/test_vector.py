"""Unit tests for vector knowledge (embedding-backed retrieval)."""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from smooth_operator_core import AgentOptions, HashEmbedder, SmoothAgent, VectorKnowledge


def test_hash_embedder_is_deterministic_and_normalized():
    emb = HashEmbedder(dim=64)
    a = emb.embed("return policy details")
    b = emb.embed("return policy details")
    assert a == b  # deterministic
    norm = sum(x * x for x in a) ** 0.5
    assert abs(norm - 1.0) < 1e-9  # L2-normalized
    assert len(a) == 64


def test_vector_knowledge_retrieves_most_similar():
    kb = VectorKnowledge(HashEmbedder(dim=256))
    kb.ingest("Our return policy allows refunds within 30 days.", "returns.md")
    kb.ingest("The office is open Monday through Friday.", "hours.md")
    hits = kb.query("how do refunds and returns work?", top_k=1)
    assert len(hits) == 1
    assert hits[0].source == "returns.md"
    assert hits[0].score > 0


def test_empty_store_returns_nothing():
    assert VectorKnowledge().query("anything", top_k=4) == []


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
async def test_agent_accepts_vector_knowledge():
    kb = VectorKnowledge()
    kb.ingest("Gift wrapping costs 4.99 per item.", "wrapping.md")
    kb.ingest("Returns are accepted within 30 days.", "returns.md")
    client = FakeClient([_resp("It's 4.99 per item.")])
    agent = SmoothAgent(client, AgentOptions(instructions="support", knowledge=kb, knowledge_top_k=1))
    await agent.run("how much is gift wrapping?")
    system = client.chat.completions.calls[0][0]["content"]
    assert "[wrapping.md]" in system
