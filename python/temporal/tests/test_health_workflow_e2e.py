"""End-to-end test of the scaffold ``HealthWorkflow`` — proves the SDK integrates
end to end (workflow -> ``health_echo`` activity -> result) against a dev server.

Self-skips if ``temporalio`` is absent or no dev server can be reached.
"""

from __future__ import annotations

import pytest

pytest.importorskip("temporalio")

from _temporal_env import temporal_client, worker
from smooth_operator_core import MockLlmProvider

from smooth_operator_temporal.temporal import AgentTurnActivities, AgentTurnWorkflow, HealthWorkflow

TASK_QUEUE = "smooth-operator-temporal-health-test"


async def test_health_workflow_echoes_end_to_end():
    # No model call happens, but the activities need an engine to construct.
    activities = AgentTurnActivities.from_engine(MockLlmProvider())

    async with (
        temporal_client() as client,
        worker(
            client,
            TASK_QUEUE,
            workflows=[AgentTurnWorkflow, HealthWorkflow],
            activities=[activities.health_echo, activities.model_call, activities.tool_invoke],
        ),
    ):
        result = await client.execute_workflow(
            HealthWorkflow.run,
            "ping",
            id="health-e2e-1",
            task_queue=TASK_QUEUE,
        )

    assert result == "smooth-operator-temporal ok: ping"
