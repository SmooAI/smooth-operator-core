"""Unit tests for streaming turn execution (``SmoothAgent.run_stream``).

Proves the streaming loop mirrors the C# ``RunStreamingAsync`` behaviour: text
deltas surface as multiple ``TextEvent``s, tool calls round-trip through the same
dispatch path, arguments split across chunks still assemble correctly, and the
terminal ``DoneEvent`` carries a response equivalent to non-streaming ``run()``.
"""

from __future__ import annotations

import json
import math

import pytest

from smooth_operator_core import (
    AgentOptions,
    DoneEvent,
    FunctionTool,
    MockLlmProvider,
    SmoothAgent,
    TextEvent,
    ToolCallEvent,
    ToolResultEvent,
    usage,
)


async def collect(agent, *args):
    return [e async for e in agent.run_stream(*args)]


@pytest.mark.asyncio
async def test_text_streams_in_multiple_chunks_then_one_done():
    mock = MockLlmProvider()
    mock.push_text("hello there friend, how are you", usage(prompt_tokens=10, completion_tokens=7))
    agent = SmoothAgent(mock, AgentOptions())

    events = await collect(agent, "hi")

    text_events = [e for e in events if isinstance(e, TextEvent)]
    assert len(text_events) >= 2
    assert "".join(e.text for e in text_events) == "hello there friend, how are you"

    done_events = [e for e in events if isinstance(e, DoneEvent)]
    assert len(done_events) == 1
    assert isinstance(events[-1], DoneEvent)
    assert done_events[0].response.text == "hello there friend, how are you"


@pytest.mark.asyncio
async def test_tool_round_trip():
    ran = {}

    async def echo(args):
        ran["text"] = args.get("text", "")
        return f"echoed:{ran['text']}"

    tool = FunctionTool(
        name="echo",
        description="Echoes input",
        parameters={"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]},
        func=echo,
    )
    mock = MockLlmProvider()
    mock.push_tool_call("call-1", "echo", '{"text":"ping"}', usage(5, 3)).push_text("all done", usage(8, 2))
    agent = SmoothAgent(mock, AgentOptions(tools=[tool]))

    events = await collect(agent, "use echo")

    tool_call = next(e for e in events if isinstance(e, ToolCallEvent))
    assert tool_call.name == "echo"
    assert json.loads(tool_call.arguments) == {"text": "ping"}
    assert ran["text"] == "ping"  # the tool actually ran

    tool_result = next(e for e in events if isinstance(e, ToolResultEvent))
    assert tool_result.name == "echo"
    assert tool_result.result == "echoed:ping"

    done = next(e for e in events if isinstance(e, DoneEvent))
    assert done.response.text == "all done"
    assert done.response.iterations == 2
    assert done.response.tool_calls == 1


@pytest.mark.asyncio
async def test_arguments_split_across_chunks_are_reassembled():
    received = {}

    async def save(args):
        received.update(args)
        return "saved"

    tool = FunctionTool(
        name="save",
        description="Saves",
        parameters={"type": "object", "properties": {"key": {"type": "string"}, "value": {"type": "string"}}},
        func=save,
    )
    mock = MockLlmProvider()
    # The mock splits these arguments across two chunks; the agent must reassemble them.
    mock.push_tool_call("call-1", "save", '{"key":"alpha","value":"beta-gamma-delta"}').push_text("ok")
    agent = SmoothAgent(mock, AgentOptions(tools=[tool]))

    await collect(agent, "save it")

    assert received == {"key": "alpha", "value": "beta-gamma-delta"}


@pytest.mark.asyncio
async def test_done_matches_run_for_same_script():
    def script():
        return MockLlmProvider().push_text("the answer is 42", usage(prompt_tokens=12, completion_tokens=6))

    opts = AgentOptions(model="claude-haiku-4-5")

    run_result = await SmoothAgent(script(), opts).run("q")
    events = await collect(SmoothAgent(script(), opts), "q")
    done = next(e for e in events if isinstance(e, DoneEvent))

    assert done.response.text == run_result.text
    assert done.response.iterations == run_result.iterations
    assert done.response.tool_calls == run_result.tool_calls
    assert done.response.usage == run_result.usage
    assert math.isclose(done.response.cost_usd, run_result.cost_usd, rel_tol=1e-9)
    assert run_result.cost_usd > 0
