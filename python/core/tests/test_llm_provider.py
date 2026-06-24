"""Unit tests for the LlmProvider seam and the reusable MockLlmProvider.

Proves the mock replays scripted text + tool-call + error responses in FIFO
order, and records the requests (messages + tool specs) the agent sent — the
behavioral parity surface of the Rust reference's ``MockLlmClient``.
"""

from __future__ import annotations

import pytest

from smooth_operator_core import (
    AgentOptions,
    FunctionTool,
    LlmProvider,
    MockLlmProvider,
    SmoothAgent,
)


@pytest.mark.asyncio
async def test_replays_text_responses_in_fifo_order():
    mock = MockLlmProvider()
    mock.push_text("first").push_text("second")

    r1 = await mock.chat.completions.create(model="m", messages=[{"role": "user", "content": "hi"}])
    r2 = await mock.chat.completions.create(model="m", messages=[{"role": "user", "content": "hi"}])

    assert r1.choices[0].message.content == "first"
    assert r2.choices[0].message.content == "second"


@pytest.mark.asyncio
async def test_records_messages_and_tools():
    mock = MockLlmProvider()
    mock.push_text("ok")
    tools = [{"type": "function", "function": {"name": "search", "description": "search", "parameters": {}}}]

    await mock.chat.completions.create(
        model="m",
        messages=[{"role": "system", "content": "be helpful"}, {"role": "user", "content": "hello"}],
        tools=tools,
    )

    assert mock.call_count == 1
    call = mock.last_call
    assert call is not None
    assert len(call.messages) == 2
    assert call.messages[0]["content"] == "be helpful"
    assert call.messages[1]["content"] == "hello"
    assert call.tools is not None
    assert call.tools[0]["function"]["name"] == "search"


@pytest.mark.asyncio
async def test_default_when_script_empty_is_benign_terminal():
    mock = MockLlmProvider()
    resp = await mock.chat.completions.create(model="m", messages=[])
    assert resp.choices[0].message.content == ""
    assert resp.choices[0].message.tool_calls is None


@pytest.mark.asyncio
async def test_scripts_errors():
    mock = MockLlmProvider()
    mock.push_error("rate limited")
    with pytest.raises(Exception, match="rate limited"):
        await mock.chat.completions.create(model="m", messages=[])


@pytest.mark.asyncio
async def test_tool_call_response_carries_the_call():
    mock = MockLlmProvider()
    mock.push_tool_call("call_1", "get_weather", '{"city": "SF"}')
    resp = await mock.chat.completions.create(model="m", messages=[])
    message = resp.choices[0].message
    assert message.tool_calls is not None
    assert message.tool_calls[0].function.name == "get_weather"
    assert message.tool_calls[0].function.arguments == '{"city": "SF"}'


def test_constructed_from_a_script_list():
    """The mock can be built from a pre-assembled script as well as fluently."""
    from smooth_operator_core import text_response

    mock = MockLlmProvider([text_response("scripted")])
    assert mock.call_count == 0  # nothing consumed yet


def test_satisfies_the_protocol():
    mock = MockLlmProvider()
    assert isinstance(mock, LlmProvider)


@pytest.mark.asyncio
async def test_drives_a_full_agent_turn_and_records_the_request():
    """End-to-end: the mock can drive the real SmoothAgent and the test asserts on
    both the replayed responses (tool-call then text) and the recorded requests."""

    async def echo(args):
        return args.get("text", "")

    tool = FunctionTool(
        name="echo",
        description="Echoes input back",
        parameters={"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]},
        func=echo,
    )
    mock = MockLlmProvider()
    mock.push_tool_call("call-1", "echo", '{"text": "hello tools"}').push_text("done")

    agent = SmoothAgent(mock, AgentOptions(tools=[tool]))
    result = await agent.run("use echo")

    assert result.text == "done"
    assert result.tool_calls == 1
    # Two model calls were recorded; the second saw the tool result fed back.
    assert mock.call_count == 2
    second_call_messages = mock.calls[1].messages
    assert any(m.get("role") == "tool" and m.get("content") == "hello tools" for m in second_call_messages)
    # The tool spec was advertised on every call.
    assert mock.calls[0].tools is not None
    assert mock.calls[0].tools[0]["function"]["name"] == "echo"
