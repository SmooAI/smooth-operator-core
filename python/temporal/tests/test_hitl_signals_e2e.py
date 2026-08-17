"""End-to-end test of **durable human-in-the-loop** via Temporal signals.

A turn whose model calls an approval-gated tool blocks the workflow until an
``approve_tool`` / ``deny_tool`` signal names that tool call. Two sequential turns
against the dev server: one **approved** (the tool runs), one **denied** (the tool
is skipped with an error result the model sees). This is the durable HITL unlock —
the block is recorded in workflow history, so it survives restarts and can resolve
arbitrarily later.

Self-skips if ``temporalio`` is absent or no dev server can be reached.
"""

from __future__ import annotations

import json

import pytest

pytest.importorskip("temporalio")

from _temporal_env import temporal_client, worker
from smooth_operator_core import AgentOptions, FunctionTool, MockLlmProvider

from smooth_operator_temporal.dto import AgentTurnInput
from smooth_operator_temporal.temporal import AgentTurnActivities, AgentTurnWorkflow

TASK_QUEUE = "smooth-operator-temporal-hitl-test"


async def _echo(args: dict) -> str:
    return str(args.get("text", ""))


ECHO = FunctionTool(
    name="echo",
    description="Echoes input back",
    parameters={"type": "object", "properties": {"text": {"type": "string"}}, "required": ["text"]},
    func=_echo,
)


async def test_hitl_gate_approves_and_denies_via_signals():
    # One mock model, scripted FIFO across BOTH (sequential) turns.
    mock = MockLlmProvider()
    mock.push_tool_call("call-approve", "echo", json.dumps({"text": "ran-after-approval"}))
    mock.push_text("done after approval")
    mock.push_tool_call("call-deny", "echo", json.dumps({"text": "should-not-run"}))
    mock.push_text("done after denial")

    activities = AgentTurnActivities.from_engine(mock, AgentOptions(tools=[ECHO]))

    def turn_input(tag: str) -> AgentTurnInput:
        return AgentTurnInput(
            system_prompt="You are a gated agent",
            user_message=f"use echo ({tag})",
            approval_required_tools=["echo"],
        )

    async with (
        temporal_client() as client,
        worker(
            client,
            TASK_QUEUE,
            workflows=[AgentTurnWorkflow],
            activities=[activities.health_echo, activities.model_call, activities.tool_invoke],
        ),
    ):
        # --- Turn 1: APPROVE ---
        approve_handle = await client.start_workflow(
            AgentTurnWorkflow.run, turn_input("approve"), id="hitl-approve", task_queue=TASK_QUEUE
        )
        # The tool-call id is deterministic from the mock script; the signal
        # buffers, so the gate sees it approved when it checks.
        await approve_handle.signal(AgentTurnWorkflow.approve_tool, "call-approve")
        approved = await approve_handle.result()

        # --- Turn 2: DENY ---
        deny_handle = await client.start_workflow(
            AgentTurnWorkflow.run, turn_input("deny"), id="hitl-deny", task_queue=TASK_QUEUE
        )
        await deny_handle.signal(AgentTurnWorkflow.deny_tool, "call-deny")
        denied = await deny_handle.result()

    # Approved turn: the gated tool actually ran, its real result is in the
    # conversation, and the turn finished.
    approved_tools = [m for m in approved if m["role"] == "tool"]
    assert len(approved_tools) == 1
    assert approved_tools[0]["content"] == "ran-after-approval"
    assert approved[-1] == {"role": "assistant", "content": "done after approval"}

    # Denied turn: the tool NEVER ran — the tool message is a denial error, not the
    # echo payload — and the model still got to wrap up.
    denied_tools = [m for m in denied if m["role"] == "tool"]
    assert len(denied_tools) == 1
    assert "denied by human approval" in denied_tools[0]["content"]
    assert denied_tools[0]["content"] != "should-not-run"
    assert denied[-1] == {"role": "assistant", "content": "done after denial"}
