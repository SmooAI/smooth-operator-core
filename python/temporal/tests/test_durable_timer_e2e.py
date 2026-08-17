"""End-to-end test of a **durable timer** — an agent that pauses itself on a
Temporal timer mid-turn, then resumes.

The model calls the configured ``wait`` tool; the workflow sleeps on
``workflow.sleep`` (recorded in history, so it survives restarts and can span
days) and then continues the turn. A short (1s) real timer against the dev server
reliably proves the durable pause without depending on time-skipping mechanics.

Self-skips if ``temporalio`` is absent or no dev server can be reached.
"""

from __future__ import annotations

import json
import time

import pytest

pytest.importorskip("temporalio")

from _temporal_env import temporal_client, worker
from smooth_operator_core import MockLlmProvider

from smooth_operator_temporal.dto import AgentTurnInput
from smooth_operator_temporal.temporal import AgentTurnActivities, AgentTurnWorkflow

TASK_QUEUE = "smooth-operator-temporal-timer-test"


async def test_durable_wait_tool_sleeps_on_a_timer_then_resumes():
    # The model asks to wait 1 second, then wraps up.
    mock = MockLlmProvider()
    mock.push_tool_call("call-wait", "wait", json.dumps({"seconds": 1})).push_text("resumed after the timer")

    # No tools registered — the `wait` tool is handled by the workflow, not the
    # tool registry/activity.
    activities = AgentTurnActivities.from_engine(mock)

    async with (
        temporal_client() as client,
        worker(
            client,
            TASK_QUEUE,
            workflows=[AgentTurnWorkflow],
            activities=[activities.health_echo, activities.model_call, activities.tool_invoke],
        ),
    ):
        started = time.monotonic()
        messages = await client.execute_workflow(
            AgentTurnWorkflow.run,
            AgentTurnInput(
                system_prompt="You are a self-pacing agent",
                user_message="wait a moment, then answer",
                wait_tool="wait",
            ),
            id="durable-timer-1",
            task_queue=TASK_QUEUE,
        )
        elapsed = time.monotonic() - started

    # The wait tool produced a durable-timer result, and the turn resumed after.
    tool_msgs = [m for m in messages if m["role"] == "tool"]
    assert len(tool_msgs) == 1
    assert "durable timer" in tool_msgs[0]["content"]
    assert messages[-1] == {"role": "assistant", "content": "resumed after the timer"}
    # It actually waited (the 1s timer elapsed), not skipped instantly.
    assert elapsed >= 0.9, f"turn returned too fast to have honored the timer: {elapsed}s"
