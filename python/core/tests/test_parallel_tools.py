"""Unit tests for concurrent (parallel) tool-call execution.

When ``AgentOptions.parallel_tool_calls`` is True and an assistant turn returns
>=2 tool calls, the dispatches run concurrently (``asyncio.gather``) — but the
tool-result messages must still be appended in the original ``tool_calls`` order
so the transcript is deterministic. Default (False) keeps sequential dispatch.
"""

from __future__ import annotations

import asyncio
from types import SimpleNamespace

import pytest

from smooth_operator_core import AgentOptions, FunctionTool, MockLlmProvider, SmoothAgent


def _multi_tool_call_response(*calls: tuple[str, str, str]) -> SimpleNamespace:
    """An OpenAI-shaped assistant message requesting several tool calls at once."""
    tool_calls = [
        SimpleNamespace(id=call_id, function=SimpleNamespace(name=name, arguments=args))
        for call_id, name, args in calls
    ]
    return SimpleNamespace(content=None, tool_calls=tool_calls)


@pytest.mark.asyncio
async def test_parallel_dispatch_overlaps():
    """Two tools that each block until both have started only complete if they ran
    concurrently — a sequential dispatch would deadlock on the barrier."""
    started = asyncio.Event()
    both_started = asyncio.Barrier(2)

    async def slow(args):
        await both_started.wait()  # blocks until BOTH tool dispatches reach here
        return args.get("text", "")

    tool_a = FunctionTool("a", "", {"type": "object"}, slow)
    tool_b = FunctionTool("b", "", {"type": "object"}, slow)
    started.set()

    mock = MockLlmProvider()
    mock.push_response(_multi_tool_call_response(("c1", "a", '{"text":"A"}'), ("c2", "b", '{"text":"B"}')))
    mock.push_text("done")

    agent = SmoothAgent(mock, AgentOptions(tools=[tool_a, tool_b], parallel_tool_calls=True))
    result = await asyncio.wait_for(agent.run("go"), timeout=5)
    assert result.text == "done"
    assert result.tool_calls == 2


@pytest.mark.asyncio
async def test_order_preserved_despite_scrambled_completion():
    """Completion order is B, C, A — the appended tool messages must still be A, B, C."""
    release = {"A": asyncio.Event(), "B": asyncio.Event(), "C": asyncio.Event()}

    async def make(name):
        async def run(args):
            await release[name].wait()
            return f"result-{name}"

        return run

    tools = [FunctionTool(n, "", {"type": "object"}, await make(n)) for n in ("A", "B", "C")]

    mock = MockLlmProvider()
    mock.push_response(_multi_tool_call_response(("c1", "A", "{}"), ("c2", "B", "{}"), ("c3", "C", "{}")))
    mock.push_text("done")

    agent = SmoothAgent(mock, AgentOptions(tools=tools, parallel_tool_calls=True))

    async def release_scrambled():
        # Finish in B, C, A order — opposite of transcript order for A.
        await asyncio.sleep(0.01)
        release["B"].set()
        await asyncio.sleep(0.01)
        release["C"].set()
        await asyncio.sleep(0.01)
        release["A"].set()

    _, _ = await asyncio.gather(agent.run("go"), release_scrambled())

    # The second model call saw the tool results; assert their order in the messages.
    second_call_messages = mock.calls[1].messages
    tool_results = [m["content"] for m in second_call_messages if m.get("role") == "tool"]
    assert tool_results == ["result-A", "result-B", "result-C"]


@pytest.mark.asyncio
async def test_failing_tool_keeps_its_position():
    async def ok(args):
        return "ok"

    async def boom(args):
        raise RuntimeError("kaboom")

    tools = [
        FunctionTool("A", "", {"type": "object"}, ok),
        FunctionTool("B", "", {"type": "object"}, boom),
        FunctionTool("C", "", {"type": "object"}, ok),
    ]
    mock = MockLlmProvider()
    mock.push_response(_multi_tool_call_response(("c1", "A", "{}"), ("c2", "B", "{}"), ("c3", "C", "{}")))
    mock.push_text("done")

    agent = SmoothAgent(mock, AgentOptions(tools=tools, parallel_tool_calls=True))
    await agent.run("go")

    tool_results = [m["content"] for m in mock.calls[1].messages if m.get("role") == "tool"]
    assert tool_results[0] == "ok"
    assert "failed" in tool_results[1] and "kaboom" in tool_results[1]
    assert tool_results[2] == "ok"


@pytest.mark.asyncio
async def test_default_off_dispatches_sequentially():
    """With the flag off, tools dispatch one at a time (in order)."""
    order: list[str] = []

    async def make(name):
        async def run(args):
            order.append(name)
            return name

        return run

    tools = [FunctionTool(n, "", {"type": "object"}, await make(n)) for n in ("A", "B")]
    mock = MockLlmProvider()
    mock.push_response(_multi_tool_call_response(("c1", "A", "{}"), ("c2", "B", "{}")))
    mock.push_text("done")

    agent = SmoothAgent(mock, AgentOptions(tools=tools))  # parallel_tool_calls defaults False
    result = await agent.run("go")
    assert order == ["A", "B"]
    assert result.tool_calls == 2


@pytest.mark.asyncio
async def test_single_tool_call_identical_with_flag_on():
    async def echo(args):
        return args.get("text", "")

    tool = FunctionTool("echo", "", {"type": "object"}, echo)
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "echo", '{"text":"hi"}').push_text("done")
    agent = SmoothAgent(mock, AgentOptions(tools=[tool], parallel_tool_calls=True))
    result = await agent.run("go")
    assert result.text == "done"
    assert result.tool_calls == 1
