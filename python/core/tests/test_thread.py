"""Unit tests for SmoothAgentThread (conversation threads across runs)."""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from smooth_operator_core import AgentOptions, SmoothAgent, SmoothAgentThread


def test_fresh_thread_has_id_and_no_messages():
    t = SmoothAgentThread()
    assert t.id  # auto-generated
    assert len(t) == 0
    assert t.messages == []


def test_thread_resumes_with_explicit_id():
    t = SmoothAgentThread(id="conv-42")
    assert t.id == "conv-42"

    # Two fresh threads get distinct ids.
    assert SmoothAgentThread().id != SmoothAgentThread().id


def test_thread_never_stores_system_messages():
    t = SmoothAgentThread()
    t.add({"role": "system", "content": "you are helpful"})
    t.add({"role": "user", "content": "hi"})
    t.extend([{"role": "system", "content": "ignored"}, {"role": "assistant", "content": "hello"}])
    roles = [m["role"] for m in t.messages]
    assert roles == ["user", "assistant"]


def _resp(content):
    msg = SimpleNamespace(content=content, tool_calls=None)
    return SimpleNamespace(choices=[SimpleNamespace(message=msg)], usage=None)


def _tool_resp(tool_id, name, arguments):
    tc = SimpleNamespace(id=tool_id, function=SimpleNamespace(name=name, arguments=arguments))
    msg = SimpleNamespace(content="", tool_calls=[tc])
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
async def test_two_runs_share_history_via_thread():
    client = FakeClient([_resp("first answer"), _resp("second answer")])
    agent = SmoothAgent(client, AgentOptions(instructions="be helpful"))
    thread = SmoothAgentThread()

    # Turn 1 — seeds nothing prior, appends [user, assistant] to the thread.
    await agent.run("hello", thread=thread)
    roles = [m["role"] for m in thread.messages]
    assert roles == ["user", "assistant"]
    assert thread.messages[0]["content"] == "hello"
    assert thread.messages[1]["content"] == "first answer"

    # Turn 2 — the second model call must see turn 1's history.
    await agent.run("again", thread=thread)
    second_call_messages = client.chat.completions.calls[1]
    contents = [m.get("content") for m in second_call_messages]
    assert "hello" in contents
    assert "first answer" in contents
    assert "again" in contents

    # The thread now holds the full 4-message conversation, no system message.
    assert [m["role"] for m in thread.messages] == ["user", "assistant", "user", "assistant"]
    assert all(m["role"] != "system" for m in thread.messages)


@pytest.mark.asyncio
async def test_first_run_sees_no_history_in_thread_mode():
    client = FakeClient([_resp("hi there")])
    agent = SmoothAgent(client, AgentOptions(instructions="be helpful"))
    thread = SmoothAgentThread()

    await agent.run("hello", thread=thread)
    # No prior turn was seeded: the thread held nothing, so the conversation that
    # accumulated is exactly this turn's user + assistant.
    assert [m["role"] for m in thread.messages] == ["user", "assistant"]
    assert thread.messages[0]["content"] == "hello"


@pytest.mark.asyncio
async def test_thread_accumulates_tool_messages():
    client = FakeClient([_tool_resp("call-1", "echo", '{"text": "hi"}'), _resp("done")])

    async def _echo(args):
        return str(args.get("text", ""))

    from smooth_operator_core import FunctionTool

    echo = FunctionTool(
        name="echo",
        description="echo",
        parameters={"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]},
        func=_echo,
    )
    agent = SmoothAgent(client, AgentOptions(tools=[echo]))
    thread = SmoothAgentThread()

    await agent.run("please echo", thread=thread)
    roles = [m["role"] for m in thread.messages]
    # user, assistant(tool_call), tool result, assistant(final answer)
    assert roles == ["user", "assistant", "tool", "assistant"]
    assert all(m["role"] != "system" for m in thread.messages)


@pytest.mark.asyncio
async def test_single_shot_run_still_works_without_thread():
    client = FakeClient([_resp("the answer is 42")])
    agent = SmoothAgent(client, AgentOptions(instructions="be helpful"))
    res = await agent.run("what is the answer?")
    assert res.text == "the answer is 42"
    assert res.iterations == 1
    assert res.tool_calls == 0
