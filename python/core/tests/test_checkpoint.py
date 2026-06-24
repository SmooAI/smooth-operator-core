"""Unit tests for conversation checkpointing."""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from smooth_operator_core import AgentOptions, Checkpoint, InMemoryCheckpointStore, SmoothAgent


def test_store_save_and_load_roundtrip():
    store = InMemoryCheckpointStore()
    assert store.load("missing") is None
    store.save(Checkpoint(conversation_id="c1", messages=[{"role": "user", "content": "hi"}]))
    loaded = store.load("c1")
    assert loaded is not None
    assert loaded.messages == [{"role": "user", "content": "hi"}]


def _resp(content):
    msg = SimpleNamespace(content=content, tool_calls=None)
    return SimpleNamespace(choices=[SimpleNamespace(message=msg)], usage=None)


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
async def test_turn_persists_and_resumes_conversation():
    store = InMemoryCheckpointStore()
    client = FakeClient([_resp("first answer"), _resp("second answer")])
    agent = SmoothAgent(client, AgentOptions(checkpoint_store=store, conversation_id="conv-1"))

    # Turn 1 — saves [user, assistant] (no system, since no instructions/knowledge).
    await agent.run("hello")
    cp = store.load("conv-1")
    assert cp is not None
    roles = [m["role"] for m in cp.messages]
    assert roles == ["user", "assistant"]
    assert cp.messages[0]["content"] == "hello"
    assert cp.messages[1]["content"] == "first answer"

    # Turn 2 — loads turn 1's conversation, so the model sees the prior history.
    await agent.run("again")
    second_call_messages = client.chat.completions.calls[1]
    contents = [m.get("content") for m in second_call_messages]
    assert "hello" in contents
    assert "first answer" in contents
    assert "again" in contents

    # The store now holds the accumulated 4-message conversation.
    cp2 = store.load("conv-1")
    assert [m["role"] for m in cp2.messages] == ["user", "assistant", "user", "assistant"]


@pytest.mark.asyncio
async def test_no_checkpoint_when_store_unset():
    store = InMemoryCheckpointStore()
    client = FakeClient([_resp("hi")])
    # conversation_id omitted -> checkpointing disabled even with a store present.
    agent = SmoothAgent(client, AgentOptions(checkpoint_store=store))
    await agent.run("hello")
    assert store.load("conv-1") is None
