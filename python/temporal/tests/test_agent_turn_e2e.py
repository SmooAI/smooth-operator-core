"""End-to-end test of a **real agent turn** running through ``AgentTurnWorkflow``
against a Temporal dev server.

The model call is backed by a ``MockLlmProvider`` injected into the activities, so
the whole per-step path is exercised: workflow -> ``model_call`` activity (mock
model) -> engine ``drive_turn`` -> returned messages. This is the proof that the
durable backend runs the same loop as the in-process path.

Self-skips if ``temporalio`` is not installed or no dev server can be reached
(offline/CI), mirroring the Rust reference's ephemeral-server-gated tests.
"""

from __future__ import annotations

import pytest

pytest.importorskip("temporalio")

from _temporal_env import temporal_client, worker
from smooth_operator_core import MockLlmProvider

from smooth_operator_temporal.dto import AgentTurnInput
from smooth_operator_temporal.temporal import AgentTurnActivities, AgentTurnWorkflow, HealthWorkflow

TASK_QUEUE = "smooth-operator-temporal-agent-turn-test"


async def test_agent_turn_workflow_runs_a_real_turn_end_to_end():
    mock = MockLlmProvider().push_text("the durable answer is 42")
    activities = AgentTurnActivities.from_engine(mock)

    async with (
        temporal_client() as client,
        worker(
            client,
            TASK_QUEUE,
            workflows=[AgentTurnWorkflow, HealthWorkflow],
            activities=[activities.health_echo, activities.model_call, activities.tool_invoke],
        ),
    ):
        messages = await client.execute_workflow(
            AgentTurnWorkflow.run,
            AgentTurnInput(system_prompt="You are a test agent", user_message="what is the durable answer?"),
            id="agent-turn-e2e-1",
            task_queue=TASK_QUEUE,
        )

    # The turn ran through the workflow: the mock's scripted reply is the final
    # assistant message, and the model was called exactly once (no tools).
    assert messages[-1] == {"role": "assistant", "content": "the durable answer is 42"}
    assert mock.call_count == 1
    # The user's message reached the model through the activity boundary.
    assert any("what is the durable answer?" == m.get("content") for m in mock.calls[0].messages)
