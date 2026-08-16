"""LLM request metadata + structured tool details — the Python port of the Rust
reference's ``ChatRequest.metadata`` / ``with_metadata`` (LiteLLM spend-log
attribution) and ``AgentEvent::ToolCallComplete.details`` (UI-facing structured
tool payload attached by a ``post_call`` hook)."""

from __future__ import annotations

import pytest

from smooth_operator_core import (
    AgentOptions,
    FunctionTool,
    MockLlmProvider,
    SmoothAgent,
    ToolResultEvent,
)
from smooth_operator_core.hooks import ToolCall, ToolHook, ToolResult


def echo_tool() -> FunctionTool:
    async def echo(args):
        return f"echoed:{args.get('text', '')}"

    return FunctionTool(
        name="echo",
        description="Echoes input",
        parameters={"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]},
        func=echo,
    )


# --- LLM request metadata ---


@pytest.mark.asyncio
async def test_metadata_absent_by_default():
    mock = MockLlmProvider()
    mock.push_text("ok")
    agent = SmoothAgent(mock, AgentOptions())
    await agent.run("hi")
    assert "metadata" not in mock.calls[0].kwargs


@pytest.mark.asyncio
async def test_metadata_forwarded_verbatim():
    mock = MockLlmProvider()
    mock.push_text("ok")
    meta = {"smooai_agent_slug": "support-bot", "k": "v"}
    agent = SmoothAgent(mock, AgentOptions(metadata=meta))
    await agent.run("hi")
    assert mock.calls[0].kwargs["metadata"] == meta


@pytest.mark.asyncio
async def test_empty_metadata_wire_identical_to_unset():
    mock = MockLlmProvider()
    mock.push_text("ok")
    agent = SmoothAgent(mock, AgentOptions(metadata={}))
    await agent.run("hi")
    assert "metadata" not in mock.calls[0].kwargs


# --- Structured tool details ---


class DetailsHook(ToolHook):
    """Attaches structured details in ``post_call`` — the same seam the Rust
    engine populates ``ToolResult.details`` through."""

    def __init__(self, details):
        self.details = details

    async def post_call(self, call: ToolCall, result: ToolResult) -> None:
        result.details = self.details


@pytest.mark.asyncio
async def test_stream_tool_result_forwards_details_from_hook():
    details = {"traceId": "abc123", "errorCount": 47}
    mock = MockLlmProvider()
    mock.push_tool_call("call-1", "echo", '{"text":"ping"}').push_text("done")
    agent = SmoothAgent(mock, AgentOptions(tools=[echo_tool()], tool_hooks=[DetailsHook(details)]))

    events = [e async for e in agent.run_stream("use echo")]

    tool_result = next(e for e in events if isinstance(e, ToolResultEvent))
    assert tool_result.result == "echoed:ping"
    assert tool_result.details == details


@pytest.mark.asyncio
async def test_stream_tool_result_details_none_without_hook():
    mock = MockLlmProvider()
    mock.push_tool_call("call-1", "echo", '{"text":"ping"}').push_text("done")
    agent = SmoothAgent(mock, AgentOptions(tools=[echo_tool()]))

    events = [e async for e in agent.run_stream("use echo")]

    tool_result = next(e for e in events if isinstance(e, ToolResultEvent))
    assert tool_result.details is None
