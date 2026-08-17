"""Unit tests for the durable-execution seam (ADR-030), mirroring the Rust
reference's ``executor.rs`` / ``activities.rs`` tests one-for-one.

They prove the two guarantees the seam exists for: the in-process executor is a
behavior-preserving pass-through over :class:`SmoothAgent`, and :func:`drive_turn`
makes the same decisions the inline loop does — one model call for a plain reply,
run-tools-and-loop for a tool call, and a hard stop at ``max_iterations``.
"""

from __future__ import annotations

import json

from smooth_operator_core import (
    AgentOptions,
    DoneEvent,
    FunctionTool,
    InProcessActivities,
    InProcessExecutor,
    MockLlmProvider,
    SmoothAgent,
    TurnPolicy,
    drive_turn,
)


async def _echo(args: dict) -> str:
    return str(args.get("text", ""))


ECHO = FunctionTool(
    name="echo",
    description="Echoes input back",
    parameters={"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]},
    func=_echo,
)


def seed(user: str) -> list[dict]:
    return [{"role": "system", "content": "You are a test agent"}, {"role": "user", "content": user}]


async def test_in_process_executor_matches_agent_run():
    """The in-process executor drives the loop identically to ``SmoothAgent.run``:
    a single text response with no tool calls ends the turn after one model call."""
    mock = MockLlmProvider().push_text("the answer is 42")
    agent = SmoothAgent(mock, AgentOptions())

    result = await InProcessExecutor().execute(agent, "what is the answer?")

    assert result.text == "the answer is 42"
    assert result.iterations == 1
    assert mock.call_count == 1
    assert any("what is the answer?" == m.get("content") for m in mock.calls[0].messages)


async def test_in_process_executor_streaming_emits_events_and_returns_result():
    """The streaming entry point surfaces events and its terminal ``DoneEvent``
    carries the same final result the awaited path returns."""
    mock = MockLlmProvider().push_text("streamed reply")
    agent = SmoothAgent(mock, AgentOptions())

    events = [e async for e in InProcessExecutor().execute_streaming(agent, "stream please")]

    assert len(events) > 1
    done = events[-1]
    assert isinstance(done, DoneEvent)
    assert done.response.text == "streamed reply"


async def test_drive_turn_text_reply_stops_after_one_model_call():
    """A plain text reply ends the turn after exactly one model call, with the
    assistant content the mock scripted — no tools run."""
    mock = MockLlmProvider().push_text("the answer is 42")
    activities = InProcessActivities(mock, AgentOptions())

    messages = seed("what is the answer?")
    await drive_turn(activities, messages, None, TurnPolicy())

    assert mock.call_count == 1
    assert messages[-1] == {"role": "assistant", "content": "the answer is 42"}


async def test_drive_turn_runs_tool_then_finishes():
    """A tool call is executed and its result appended — paired to the call by id
    and name — then the follow-up text reply ends the turn."""
    mock = MockLlmProvider()
    mock.push_tool_call("call-1", "echo", json.dumps({"text": "hello tools"})).push_text("done")
    activities = InProcessActivities(mock, AgentOptions(tools=[ECHO]))

    messages = seed("use the echo tool")
    await drive_turn(activities, messages, None, TurnPolicy())

    assert mock.call_count == 2
    tool_msgs = [m for m in messages if m["role"] == "tool"]
    assert len(tool_msgs) == 1
    assert tool_msgs[0]["content"] == "hello tools"
    assert tool_msgs[0]["tool_call_id"] == "call-1"
    assert tool_msgs[0]["name"] == "echo"
    assert messages[-1] == {"role": "assistant", "content": "done"}


async def test_drive_turn_max_iterations_bounds_loop():
    """An unending tool chain stops at ``max_iterations`` — the budget is
    exhausted, not an error: what accumulated so far is left on ``messages``."""
    mock = MockLlmProvider()
    for i in range(5):
        mock.push_tool_call(f"call-{i}", "echo", json.dumps({"text": f"loop {i}"}))
    activities = InProcessActivities(mock, AgentOptions(tools=[ECHO]))

    messages = seed("loop forever")
    await drive_turn(activities, messages, None, TurnPolicy(max_iterations=2))

    assert mock.call_count == 2
    assert len([m for m in messages if m["role"] == "tool"]) == 2
