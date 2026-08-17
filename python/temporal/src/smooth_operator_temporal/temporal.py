"""Temporal-backed durable execution (requires the ``temporalio`` SDK).

An agent turn runs as :class:`AgentTurnWorkflow`, whose side-effects — the model
call and each tool invocation — are Temporal **activities**
(:class:`AgentTurnActivities`). The workflow drives the engine's deterministic
:func:`~smooth_operator_core.drive_turn` orchestration **unchanged** via a
:class:`_WorkflowActivities` adapter that schedules those activities on the
workflow. The in-process path runs the same ``drive_turn`` inline — one loop, two
backends.

This module imports ``temporalio``; importing it therefore requires the optional
``temporal`` extra (``pip install 'smooai-smooth-operator-temporal[temporal]'``),
mirroring how the Rust reference puts all workflow/activity wiring behind an
off-by-default cargo feature. The serde :mod:`.dto` boundary imports no Temporal
SDK and is always available.

Engine handles (the model provider + tool set the activities run against) are held
on the :class:`AgentTurnActivities` **instance** and injected at worker
construction — Python has no free-function-only activity constraint, so this
replaces the Rust reference's process-global ``init_engine`` with plain dependency
injection. The activities reuse the engine's
:class:`~smooth_operator_core.InProcessActivities` as their dispatch surface, so
the durable path's model/tool behavior is identical to the inline loop's.
"""

from __future__ import annotations

import json
import logging
from datetime import timedelta
from typing import Any

from temporalio import activity, workflow

# `drive_turn` is pure/deterministic (no I/O, clock or RNG), so it is safe to run
# as workflow code. Pass its imports (and the engine dispatch surface the
# activities use) through the workflow sandbox unchanged rather than have the
# sandbox re-import the whole engine + `openai`.
with workflow.unsafe.imports_passed_through():
    from smooth_operator_core import (
        AgentOptions,
        InProcessActivities,
        LlmProvider,
        ToolResult,
        TurnPolicy,
        drive_turn,
    )

    from .dto import AgentTurnInput, ModelCallInput, ModelCallOutput, ToolInvokeInput, ToolInvokeOutput

logger = logging.getLogger(__name__)

#: Start-to-close timeout for the model/tool activities, mirroring the Rust
#: reference's 60s default. (Retry-policy tuning lands with the per-step retry work.)
_ACTIVITY_TIMEOUT = timedelta(seconds=60)


class AgentTurnActivities:
    """The side-effecting boundary of a turn, as Temporal activities.

    Holds an :class:`~smooth_operator_core.InProcessActivities` — the engine's own
    dispatch surface (retry/backoff, ``max_tokens`` clamping, tool resolution with
    clearance + hooks) — so the ``model_call`` / ``tool_invoke`` activities behave
    exactly as the inline loop does. Register its bound methods on a
    :class:`temporalio.worker.Worker`.
    """

    def __init__(self, engine: InProcessActivities) -> None:
        self._engine = engine

    @classmethod
    def from_engine(cls, llm: LlmProvider, options: AgentOptions | None = None) -> AgentTurnActivities:
        """Build activities backed by a fresh engine dispatch surface over ``llm``."""
        return cls(InProcessActivities(llm, options or AgentOptions()))

    @activity.defn
    async def health_echo(self, message: str) -> str:
        """Liveness probe backing :class:`HealthWorkflow`."""
        return f"smooth-operator-temporal ok: {message}"

    @activity.defn
    async def model_call(self, input: ModelCallInput) -> ModelCallOutput:
        """The model call — the ``Think`` step — run as a durable activity."""
        assistant = await self._engine.model_call(input.messages, input.tools)
        return ModelCallOutput.from_assistant(assistant)

    @activity.defn
    async def tool_invoke(self, input: ToolInvokeInput) -> ToolInvokeOutput:
        """A single tool invocation — the ``Act`` step — run as a durable activity.

        Tool *business* failures come back on ``is_error`` (not as a raise),
        matching the engine's own dispatch.
        """
        result = await self._engine.tool_invoke(input.call)
        return ToolInvokeOutput(content=result.content, is_error=result.is_error)


def _wait_seconds(call: dict[str, Any]) -> int | None:
    """Extract a non-negative integer ``seconds`` from a wire tool call, or ``None``.

    The wire arguments are a JSON string (the shape the model emits); ``bool`` is
    rejected even though it is an ``int`` subclass in Python.
    """
    raw = call.get("function", {}).get("arguments")
    try:
        args = json.loads(raw) if isinstance(raw, str) else (raw or {})
    except (ValueError, TypeError):
        return None
    seconds = args.get("seconds") if isinstance(args, dict) else None
    if isinstance(seconds, bool) or not isinstance(seconds, int) or seconds < 0:
        return None
    return seconds


class _WorkflowActivities:
    """Makes the engine's ``AgentActivities`` surface schedule Temporal activities.

    ``approval_required`` names tools that block **durably** on an approval signal
    before they run (the HITL unlock — no mid-turn connection state, just a
    signal). ``wait_tool`` (if set) names a built-in **durable wait**: a model call
    to it sleeps the workflow on a Temporal timer, a pause that survives restarts
    and can span days, instead of dispatching an activity.
    """

    def __init__(self, wf: AgentTurnWorkflow, approval_required: list[str], wait_tool: str | None) -> None:
        self._wf = wf
        self._approval_required = set(approval_required)
        self._wait_tool = wait_tool

    async def model_call(self, messages: list[dict[str, Any]], tools: list[dict[str, Any]] | None) -> dict[str, Any]:
        output: ModelCallOutput = await workflow.execute_activity_method(
            AgentTurnActivities.model_call,
            ModelCallInput(messages=messages, tools=tools),
            start_to_close_timeout=_ACTIVITY_TIMEOUT,
        )
        return output.to_assistant()

    async def tool_invoke(self, call: dict[str, Any]) -> ToolResult:
        name = call["function"]["name"]
        call_id = call["id"]

        # Durable wait: sleep on a Temporal timer instead of dispatching an
        # activity. The timer is recorded in workflow history, so the pause
        # survives restarts and can span days.
        if self._wait_tool is not None and name == self._wait_tool:
            seconds = _wait_seconds(call)
            if seconds is None:
                return ToolResult(content=f"wait tool '{name}' requires an integer `seconds` argument", is_error=True)
            await workflow.sleep(timedelta(seconds=seconds))
            return ToolResult(content=f"Waited {seconds}s (durable timer).", is_error=False)

        # Durable human-in-the-loop gate: block until an approve_tool / deny_tool
        # signal names this call id. The wait lives in workflow history, so it
        # survives restarts and can resolve hours later. `drive_turn` is unchanged
        # — the gate lives here.
        if name in self._approval_required:
            await workflow.wait_condition(lambda: call_id in self._wf.approved or call_id in self._wf.denied)
            if call_id in self._wf.denied:
                return ToolResult(content=f"Tool call '{name}' was denied by human approval.", is_error=True)

        output: ToolInvokeOutput = await workflow.execute_activity_method(
            AgentTurnActivities.tool_invoke,
            ToolInvokeInput(call=call),
            start_to_close_timeout=_ACTIVITY_TIMEOUT,
        )
        return ToolResult(content=output.content, is_error=output.is_error)


@workflow.defn
class AgentTurnWorkflow:
    """One agent turn as a Temporal workflow.

    Seeds the conversation, then drives the engine's :func:`~smooth_operator_core.drive_turn`
    unchanged over :class:`_WorkflowActivities`, so the durable path is the same
    loop as in-process. Returns the full message list.

    Workflow state is the human-approval ledger — ``approved`` / ``denied``
    tool-call ids, populated by the ``approve_tool`` / ``deny_tool`` signals and
    observed by the adapter's durable gate.
    """

    def __init__(self) -> None:
        self.approved: set[str] = set()
        self.denied: set[str] = set()

    @workflow.run
    async def run(self, input: AgentTurnInput) -> list[dict[str, Any]]:
        messages: list[dict[str, Any]] = [
            {"role": "system", "content": input.system_prompt},
            {"role": "user", "content": input.user_message},
        ]
        policy = TurnPolicy() if input.max_iterations == 0 else TurnPolicy(max_iterations=input.max_iterations)
        adapter = _WorkflowActivities(self, input.approval_required_tools, input.wait_tool)
        await drive_turn(adapter, messages, input.tools or None, policy)
        return messages

    @workflow.signal
    def approve_tool(self, call_id: str) -> None:
        """A human approves the tool call with this id, unblocking the gate."""
        self.approved.add(call_id)

    @workflow.signal
    def deny_tool(self, call_id: str) -> None:
        """A human denies the tool call with this id; the gate returns a tool-error
        result instead of running it."""
        self.denied.add(call_id)


@workflow.defn
class HealthWorkflow:
    """Scaffold liveness workflow — proves the SDK integrates end to end and backs
    the ephemeral-server integration test."""

    @workflow.run
    async def run(self, message: str) -> str:
        return await workflow.execute_activity_method(
            AgentTurnActivities.health_echo,
            message,
            start_to_close_timeout=timedelta(seconds=10),
        )
