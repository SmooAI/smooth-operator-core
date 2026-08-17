"""The durable-execution seam (ADR-030): *where* a turn runs, and the
deterministic orchestration it runs.

Two boundaries live here, and they are deliberately separate:

* :class:`AgentExecutor` — the coarse dispatch seam. It decides where and how a
  whole turn runs while :class:`~smooth_operator_core.agent.SmoothAgent` stays the
  unit of orchestration (instructions, tools, budget, compaction, checkpointing).
  The only implementation shipped here is :class:`InProcessExecutor`, a verbatim
  delegation to :meth:`SmoothAgent.run` / :meth:`SmoothAgent.run_stream` — so
  introducing the seam changes no behavior, carries no dependency and needs no
  infrastructure. Both methods take the agent as a parameter: an executor borrows
  the agent for the turn, it never owns it.

* :class:`AgentActivities` + :func:`drive_turn` — the fine-grained split. The
  *side-effecting* half (the model call, each tool invocation) is what a durable
  backend runs as retried, memoized activities; the *deterministic* half is the
  loop that decides "call the model → if it asked for tools, run them and loop →
  else stop." :func:`drive_turn` performs no I/O, reads no wall clock and draws no
  randomness, which is what makes it replay-safe: a durable workflow can run this
  very function, and the in-process path runs it too. One loop, two backends — that
  is what keeps a durable path from drifting away from the inline one.

Everything crossing the activity boundary is a plain, serializable value: messages
and tool specs are OpenAI-wire dicts, the model call returns the assistant message
as a dict, and a tool call is the wire-shape ``{"id", "type", "function"}`` dict the
model emitted. Nothing here holds a live client, socket or generator.

Scope, mirroring the Rust reference: :func:`drive_turn` reproduces the *core*
decision flow of :meth:`SmoothAgent.run` (snapshot context → model call → append
assistant message → stop-or-run-tools → append results → loop to
``max_iterations``). It deliberately omits the inline-runtime concerns a durable
backend models differently or that are follow-ups: stream event emission,
checkpointing (workflow history *is* the checkpoint), compaction, budget
enforcement, parallel tool dispatch, knowledge/memory injection. Those layer onto
this loop without changing its shape.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, AsyncIterator, Protocol

from .agent import AgentOptions, AgentRunResponse, SmoothAgent, StreamEvent
from .hooks import ToolResult
from .llm_provider import LlmProvider
from .thread import SmoothAgentThread


class AgentExecutor(Protocol):
    """A backend that executes one agent turn.

    Implementations decide *how* the turn is driven (inline, or on a durable
    workflow engine); the :class:`~smooth_operator_core.agent.SmoothAgent` passed in
    owns *what* the turn does. The signatures mirror the agent's own entry points
    exactly, so an executor is a drop-in for calling the agent directly.
    """

    async def execute(
        self,
        agent: SmoothAgent,
        message: str,
        history: list[dict[str, Any]] | None = None,
        thread: SmoothAgentThread | None = None,
    ) -> AgentRunResponse:
        """Run a single turn, returning the same result :meth:`SmoothAgent.run` would.

        Propagates any fatal error from the underlying turn (model call, tool
        dispatch, or — for a durable backend — the workflow engine).
        """
        ...

    def execute_streaming(
        self,
        agent: SmoothAgent,
        message: str,
        history: list[dict[str, Any]] | None = None,
        thread: SmoothAgentThread | None = None,
    ) -> AsyncIterator[StreamEvent]:
        """Run a single turn, yielding :data:`~smooth_operator_core.agent.StreamEvent`s
        as work happens (text deltas, tool call/result, one terminal ``DoneEvent``).

        Not a coroutine: like :meth:`SmoothAgent.run_stream` this *returns* the async
        iterator rather than awaiting a result.
        """
        ...


class InProcessExecutor:
    """The default, zero-infra executor: drives the turn inline in the calling task
    by delegating straight to :meth:`SmoothAgent.run` / :meth:`SmoothAgent.run_stream`.

    A verbatim pass-through — introducing it changes no behavior. It is the executor
    used unless a consumer explicitly opts into a durable backend.
    """

    async def execute(
        self,
        agent: SmoothAgent,
        message: str,
        history: list[dict[str, Any]] | None = None,
        thread: SmoothAgentThread | None = None,
    ) -> AgentRunResponse:
        return await agent.run(message, history, thread)

    def execute_streaming(
        self,
        agent: SmoothAgent,
        message: str,
        history: list[dict[str, Any]] | None = None,
        thread: SmoothAgentThread | None = None,
    ) -> AsyncIterator[StreamEvent]:
        return agent.run_stream(message, history, thread)


# TODO(ADR-030): the Temporal-backed AgentExecutor/AgentActivities belongs in a
# SEPARATE distribution behind an optional extra (mirroring the Rust reference's
# separate `smooth-operator-temporal` crate and its off-by-default `temporal` cargo
# feature), so this published package stays zero-infra and never pulls a
# `temporalio` dependency. It implements these same two Protocols — scheduling
# `model_call` / `tool_invoke` as activities — then calls `drive_turn` unchanged.
class AgentActivities(Protocol):
    """The side-effecting boundary of a turn: the model call and tool invocations.

    A durable backend runs each of these as an activity (retried upstream, memoized
    in history); the in-process backend runs them inline. Inputs and outputs are
    plain OpenAI-wire values — dicts and a :class:`~smooth_operator_core.hooks.ToolResult`
    — so an implementation may serialize them across an activity boundary unchanged.
    """

    async def model_call(self, messages: list[dict[str, Any]], tools: list[dict[str, Any]] | None) -> dict[str, Any]:
        """Invoke the model with the given context and visible tool specs, returning
        the assistant message in OpenAI wire shape (``role``/``content``, plus
        ``tool_calls`` when the model asked for any).

        Propagates a fatal model-call error (network, upstream rejection). A durable
        backend turns transient failures into activity retries before this surfaces.
        """
        ...

    async def tool_invoke(self, call: dict[str, Any]) -> ToolResult:
        """Execute a single wire-shape tool call, returning its result.

        Raises only on a fatal *dispatch* error. Tool **business** failures are
        reported on the returned result's
        :attr:`~smooth_operator_core.hooks.ToolResult.is_error` — never as an
        exception — mirroring how the agent's own dispatch feeds a failing tool back
        to the model instead of crashing the turn.
        """
        ...


@dataclass(frozen=True)
class TurnPolicy:
    """Loop policy for :func:`drive_turn`: the bound that keeps the orchestration
    terminating. Mirrors :attr:`~smooth_operator_core.agent.AgentOptions.max_iterations`
    — which defaults to 8 here, where the Rust reference's ``AgentConfig`` defaults
    to 50; each language keeps its own engine's default.
    """

    max_iterations: int = 8


async def drive_turn(
    activities: AgentActivities,
    messages: list[dict[str, Any]],
    tools: list[dict[str, Any]] | None,
    policy: TurnPolicy,
) -> None:
    """Drive one agent turn deterministically over ``activities``, mutating
    ``messages`` in place.

    ``messages`` must already be seeded with the system prompt, any prior turns and
    the current user message — exactly the state :meth:`SmoothAgent.run` holds when
    it enters its loop. On return it carries the appended assistant and tool
    messages.

    This performs no I/O, wall-clock read or RNG of its own — all of that is
    delegated to ``activities`` — so it is safe to run as durable workflow code
    (replay-deterministic) as well as inline. It propagates the first fatal error
    from either activity; an exhausted iteration budget is not an error, it simply
    returns what has accumulated so far.
    """
    for _iteration in range(1, policy.max_iterations + 1):
        context = list(messages)

        response = await activities.model_call(context, tools)

        content = response.get("content") or ""
        tool_calls = response.get("tool_calls") or []

        # The Rust push condition is content OR tool calls OR reasoning content;
        # the OpenAI wire shape this engine speaks carries no reasoning field.
        if content or tool_calls:
            assistant: dict[str, Any] = {"role": "assistant", "content": content}
            if tool_calls:
                assistant["tool_calls"] = tool_calls
            messages.append(assistant)

        if not tool_calls:
            return

        for call in tool_calls:
            result = await activities.tool_invoke(call)
            messages.append(
                {
                    "role": "tool",
                    "tool_call_id": call["id"],
                    "name": call["function"]["name"],
                    "content": result.content,
                }
            )


class InProcessActivities:
    """The default, zero-infra :class:`AgentActivities`: runs the model call through
    an :class:`~smooth_operator_core.llm_provider.LlmProvider` and tool calls through
    the configured tool set, inline in the calling task.

    It holds a :class:`~smooth_operator_core.agent.SmoothAgent` built from the same
    provider and options purely as the *dispatch surface* — that is where this engine
    keeps the equivalents of the Rust reference's ``LlmProvider`` and ``ToolRegistry``
    (retry/backoff, ``max_tokens`` clamping, metadata, then tool resolution with
    clearance, human gate and the tool hooks). Reusing it is what makes the activity
    path behave identically to the inline loop; the agent's own turn loop is not used.
    """

    def __init__(self, llm: LlmProvider, options: AgentOptions) -> None:
        self._agent = SmoothAgent(llm, options)

    async def model_call(self, messages: list[dict[str, Any]], tools: list[dict[str, Any]] | None) -> dict[str, Any]:
        response = await self._agent._call_model(messages, tools)
        choice = response.choices[0].message
        assistant: dict[str, Any] = {"role": "assistant", "content": choice.content or ""}
        if choice.tool_calls:
            assistant["tool_calls"] = [
                {
                    "id": tc.id,
                    "type": "function",
                    "function": {"name": tc.function.name, "arguments": tc.function.arguments},
                }
                for tc in choice.tool_calls
            ]
        return assistant

    async def tool_invoke(self, call: dict[str, Any]) -> ToolResult:
        function = call["function"]
        return await self._agent._dispatch_tool_result(function["name"], function.get("arguments") or "", None)
