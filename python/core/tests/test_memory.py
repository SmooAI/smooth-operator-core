"""Unit tests for long-term memory."""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from smooth_operator_core import AgentOptions, InMemoryMemory, SmoothAgent


def test_remember_and_recall_by_overlap():
    mem = InMemoryMemory()
    mem.remember("The user's name is Dana.")
    mem.remember("The user prefers metric units.")
    mem.remember("Gift wrapping costs 4.99.")
    recalled = mem.recall("what units does the user prefer?", top_k=1)
    assert len(recalled) == 1
    assert "metric" in recalled[0].text


def test_recall_returns_nothing_on_no_overlap():
    mem = InMemoryMemory()
    mem.remember("The sky is blue.")
    assert mem.recall("quarterly revenue forecast", top_k=4) == []


def test_blank_memory_is_ignored():
    mem = InMemoryMemory()
    mem.remember("   ")
    assert mem.recall("anything", top_k=4) == []


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
async def test_recalled_memory_is_injected_into_system_prompt():
    mem = InMemoryMemory()
    mem.remember("The user's name is Dana.")
    mem.remember("Unrelated trivia about penguins.")
    client = FakeClient([_resp("Hi Dana!")])
    agent = SmoothAgent(client, AgentOptions(instructions="support", memory=mem))
    await agent.run("do you remember my name?")
    system = client.chat.completions.calls[0][0]["content"]
    assert "Relevant memory" in system
    assert "Dana" in system
    # The unrelated entry has no overlap with the query, so it is not recalled.
    assert "penguins" not in system
