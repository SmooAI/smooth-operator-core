"""The Python smooth-operator core: a native agentic loop.

Phase-0 sibling of the C# ``SmoothAgent`` (``dotnet/core``) and the Rust
reference engine. Drives an agentic tool-calling loop over any OpenAI-compatible
chat client (the ``openai`` SDK pointed at a gateway): inject retrieved
knowledge, call the model, run any requested tools, feed results back, and loop
until the model answers without a tool call or the iteration budget is hit.

Phase 1 adds context compaction and token/cost budgeting; further features
(checkpointing, rerank, memory, sub-agents, vector knowledge) layer on as they did
when the C# core grew past Phase 0.
"""

from __future__ import annotations

import asyncio
import inspect
import json
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any, AsyncIterator, Awaitable, Callable, Protocol, Union

from .cache_control import apply_cache_control, supports_anthropic_cache_control
from .cast import Clearance
from .checkpoint import Checkpoint, CheckpointStore
from .compaction import compact
from .cost import CostBudget, CostTracker, ModelPricing, Usage, parse_gateway_cost
from .deny_policy import DenyPolicy
from .hooks import ToolCall, ToolHook, ToolResult
from .human_gate import HumanApprovalRequest, HumanGate
from .knowledge import Knowledge
from .memory import Memory
from .multimodal import ImageContent, user_content
from .permission import AutoMode, PermissionHook
from .rerank import NoopReranker, Reranker
from .thread import SmoothAgentThread
from .tool_search import ToolSearch

if TYPE_CHECKING:  # import only for typing — never at runtime
    from .extension.host import ExtensionHost

#: SEP event names the agent loop emits. Mirrors the Rust host's
#: ``extension::events`` constants.
_SEP_TURN_START = "turn_start"
_SEP_TURN_END = "turn_end"
_SEP_MESSAGE_END = "message_end"


def effective_max_tokens(max_tokens: int, model_max_output: int | None) -> int:
    """The ``max_tokens`` to actually send: the configured budget, clamped DOWN to
    the model's hard output ceiling when one is known.

    A policy/budget ``max_tokens`` (tuned high, or raised per-org) can exceed what
    a model can physically emit — a reasoning model then burns the whole budget on
    reasoning and returns EMPTY, or the upstream 400s (e.g. ``groq-compound`` caps
    output at 8192). Clamping to ``min(max_tokens, ceiling)`` prevents that.

    ``model_max_output`` of ``None`` (or ``<= 0``) means the ceiling is unknown ⇒
    graceful passthrough (no clamp). Never returns 0 — a 0 budget would make the
    model emit nothing. Mirrors the Rust reference's
    ``LlmClient::effective_max_tokens`` (EPIC th-1cc9fa / th-562b6d)."""
    if model_max_output is None or model_max_output <= 0:
        return max_tokens
    return max(1, min(max_tokens, model_max_output))


class Tool(Protocol):
    """A callable tool the agent may invoke. Mirrors the reference engines' tool seam."""

    name: str
    description: str
    parameters: dict[str, Any]

    async def execute(self, arguments: dict[str, Any]) -> str: ...


@dataclass
class FunctionTool:
    """Wrap an ordinary async function as a :class:`Tool` (akin to AIFunctionFactory)."""

    name: str
    description: str
    parameters: dict[str, Any]
    func: Callable[[dict[str, Any]], Awaitable[str]]

    async def execute(self, arguments: dict[str, Any]) -> str:
        return await self.func(arguments)


@dataclass
class AgentOptions:
    """Configuration for a :class:`SmoothAgent` turn. Mirrors the C# ``AgentOptions``."""

    instructions: str = ""
    model: str = "claude-haiku-4-5"
    max_iterations: int = 8
    max_tokens: int = 512
    #: The active model's hard **output** ceiling (``max_output_tokens``), when
    #: known. Requests clamp ``max_tokens`` to ``min(max_tokens, ceiling)`` (see
    #: :func:`effective_max_tokens`) so a budget/policy ``max_tokens`` can never
    #: exceed what the model can physically emit — otherwise a reasoning model
    #: burns its budget and returns empty, or the upstream 400s. ``None`` (the
    #: default) ⇒ unknown ⇒ no clamp (graceful passthrough). The server sources it
    #: from the gateway's ``/model/info`` (EPIC th-1cc9fa). Mirrors the Rust
    #: reference's ``AgentConfig.model_max_output`` / ``with_model_ceiling``.
    model_max_output: int | None = None
    #: Opaque mapping forwarded verbatim as every model request's top-level
    #: ``metadata`` field (LiteLLM records it on spend logs — e.g. an agent slug
    #: so per-agent LLM spend is queryable at the gateway). ``None`` or empty ⇒
    #: the kwarg is never sent, byte-identical to unset. Mirrors the Rust
    #: engine's ``AgentConfig.with_metadata``.
    metadata: dict[str, Any] | None = None
    temperature: float = 0.0
    knowledge: Knowledge | None = None
    knowledge_top_k: int = 4
    #: Reranker applied to retrieved hits before injection (defaults to passthrough).
    reranker: Reranker = field(default_factory=NoopReranker)
    #: Candidate pool size to retrieve before reranking. When greater than
    #: ``knowledge_top_k``, more documents are fetched, reranked, and trimmed to
    #: ``knowledge_top_k`` — so the reranker can promote a better candidate.
    knowledge_candidate_k: int = 0
    #: Optional long-term memory; relevant entries are recalled into context each turn.
    memory: Memory | None = None
    #: How many memory entries to recall per turn.
    memory_top_k: int = 4
    tools: list[Tool] = field(default_factory=list)
    #: Tool-call lifecycle hooks (parity with the Rust ``ToolHook`` trait). Every
    #: hook's ``pre_call`` runs before a tool executes (raise to block it) and its
    #: ``post_call`` runs after with a mutable :class:`~.hooks.ToolResult` — the
    #: redaction seam (a hook may rewrite ``result.content`` and that mutation is
    #: what the model/transcript sees). Empty (the default) ⇒ no hooks, zero
    #: overhead, behavior unchanged.
    tool_hooks: list[ToolHook] = field(default_factory=list)
    #: Tool-call permission mode (pearl th-ab0437 — parity with the Rust engine's
    #: ``AutoMode``). When set (not ``None``), a :class:`~.permission.PermissionHook`
    #: enforcing this mode is prepended to :attr:`tool_hooks` so it gates every call
    #: FIRST (read-only allow / mutating ask / dangerous deny, per the mode). ``None``
    #: (the default) ⇒ no permission gating (behavior unchanged). For interactive
    #: ``Ask`` routing or a persisted allow-list, construct a ``PermissionHook``
    #: directly (``.with_approver`` / ``.with_grants``) and add it to ``tool_hooks``.
    permission_mode: AutoMode | None = None
    #: Consumer deny policy (pearl th-deny-policy — parity with the Rust
    #: ``DenyPolicy``). When set, its rules/predicates are enforced FIRST as hard
    #: circuit-breakers (no grant waives them, no mode downgrades them). If
    #: :attr:`permission_mode` is also ``None``, the policy alone still activates a
    #: gate in :attr:`~.permission.AutoMode.BYPASS` mode — only the built-in
    #: circuit-breakers and this policy deny; everything else is allowed. ``None``
    #: (the default) ⇒ no deny policy (behavior unchanged).
    deny_policy: DenyPolicy | None = None
    #: When True and an assistant turn returns >=2 tool calls, dispatch them
    #: concurrently (``asyncio.gather``) instead of sequentially. Tool-result
    #: messages are still appended in the original ``tool_calls`` order, so the
    #: transcript stays deterministic regardless of completion order. Default
    #: False preserves the sequential behaviour. Per-tool semantics (clearance,
    #: human-gate approval, tool_search promotion, JSON parsing, error handling)
    #: are unchanged — only the dispatch loop runs in parallel.
    parallel_tool_calls: bool = False
    #: Deferred tools — registered but with their schemas HIDDEN from the model.
    #: When any are present, a built-in ``tool_search`` meta-tool is advertised in
    #: their place; the model calls it to fuzzy-match and promote the ones it needs,
    #: which then become visible + dispatchable on subsequent turns. Keeps the tool
    #: schema payload small when there are many rarely-used tools. An unpromoted
    #: deferred tool is NOT dispatchable.
    deferred_tools: list[Tool] = field(default_factory=list)
    #: Image attachments for the CURRENT turn's user message (a multimodal turn).
    #: Set by a host that received a chat turn carrying images; emitted as OpenAI
    #: ``image_url`` content parts on that one turn. Empty (the default) leaves every
    #: text-only turn byte-identical. Mirrors Rust's ``AgentConfig::with_user_images``.
    next_user_images: list[ImageContent] = field(default_factory=list)
    #: A loaded SEP :class:`~smooth_operator_core.extension.host.ExtensionHost` to
    #: wire into this agent — the Python sibling of Rust's
    #: ``Agent::with_extension_host``. Its tools are merged into the agent's tool
    #: set and its hook lanes run at the same points Rust runs them. ``None`` (the
    #: default) leaves the loop exactly as it was before extensions existed.
    extensions: ExtensionHost | None = None
    #: Approximate token budget for the context window. Before each model call,
    #: older non-system messages are dropped (sliding window) to stay under it.
    #: ``0`` disables compaction.
    max_context_tokens: int = 8000
    #: Optional ceiling for the turn (token and/or USD). The turn stops early once
    #: a model call pushes accumulated usage/cost over the budget.
    budget: CostBudget | None = None
    #: Per-model pricing override for cost accounting (defaults to DEFAULT_PRICING).
    pricing: dict[str, ModelPricing] | None = None
    #: Optional store for persisting/resuming the conversation. When set together
    #: with ``conversation_id``, prior messages are loaded at the start of a turn
    #: and the updated conversation is saved at the end.
    checkpoint_store: CheckpointStore | None = None
    #: Conversation id for the checkpoint store (required to use checkpointing).
    conversation_id: str | None = None
    #: Optional tool-access policy. When set, a tool the clearance forbids is not
    #: dispatched — a "tool not permitted" result is returned to the model instead.
    #: ``None`` allows every tool (the prior behaviour).
    clearance: Clearance | None = None
    #: Optional human-in-the-loop gate. When set, the agent asks it for approval
    #: before running any tool call for which ``requires_approval`` returns true.
    #: A denied call is not executed; the model is told it was denied and can adapt.
    human_gate: HumanGate | None = None
    #: Which tool calls need human approval (e.g. writes / destructive actions),
    #: given the tool name and parsed arguments. Default: none. Only consulted when
    #: ``human_gate`` is set. Example::
    #:
    #:     lambda name, args: name in {"delete_record", "send_email"}
    requires_approval: Callable[[str, dict[str, Any]], bool] | None = None
    #: Number of ADDITIONAL attempts after the first if the model call raises a
    #: transient error (rate-limit, 5xx, dropped connection). ``0`` (the default)
    #: preserves today's behaviour: a single attempt, error propagates immediately.
    #: Only the model call is retried — never tool execution.
    max_retries: int = 0
    #: Base delay (milliseconds) for exponential backoff between retries. The wait
    #: before retry attempt ``n`` (1-indexed) is ``retry_backoff_ms * 2 ** (n - 1)``.
    #: Set to ``0`` to retry without sleeping (used by tests).
    retry_backoff_ms: int = 200


@dataclass
class AgentRunResponse:
    """The result of a turn: the final assistant text plus a little provenance."""

    text: str
    iterations: int
    tool_calls: int
    usage: Usage = field(default_factory=Usage)
    cost_usd: float = 0.0
    #: True if the turn stopped because the cost/token budget was hit.
    budget_exceeded: bool = False


@dataclass(frozen=True)
class TextEvent:
    """An incremental assistant content delta as it streams in."""

    text: str
    type: str = "text"


@dataclass(frozen=True)
class ToolCallEvent:
    """A tool call the model requested, emitted once before it is dispatched."""

    name: str
    arguments: str
    type: str = "tool_call"


@dataclass(frozen=True)
class ToolResultEvent:
    """A tool's result, emitted after it finishes.

    ``details`` carries the structured, UI-facing payload a ``post_call`` hook
    attached to the :class:`~smooth_operator_core.hooks.ToolResult` (``None``
    when none) — forwarded verbatim and un-truncated, never shown to the model.
    Mirrors the Rust engine's ``AgentEvent::ToolCallComplete.details``.
    """

    name: str
    result: str
    details: Any | None = None
    type: str = "tool_result"


@dataclass(frozen=True)
class DoneEvent:
    """The single terminal event, carrying the same :class:`AgentRunResponse`
    that :meth:`SmoothAgent.run` would return for the same script."""

    response: AgentRunResponse
    type: str = "done"


#: A streamed event from :meth:`SmoothAgent.run_stream`. A tagged union (each variant
#: carries a literal ``type``), mirroring the C# ``RunStreamingAsync`` update sequence
#: and the Rust reference engine's event stream.
StreamEvent = Union[TextEvent, ToolCallEvent, ToolResultEvent, DoneEvent]


def _response_gateway_cost(response: Any) -> float | None:
    """The gateway's authoritative cost for a response, when the client surfaced one.

    The engine takes an injected OpenAI-compatible client, and the SDK's parsed
    response carries no headers — the cost lives ONLY in a response header. So this
    reads two shapes, in order: a ``gateway_cost_usd`` a wrapping client already
    parsed and attached, or raw ``headers`` hanging off the response (what
    ``client.chat.completions.with_raw_response`` gives you). Absent both, ``None`` —
    unmeasured, and the local pricing estimate is used instead of a bogus $0.
    """
    attached = getattr(response, "gateway_cost_usd", None)
    if isinstance(attached, (int, float)) and attached > 0:
        return float(attached)
    return parse_gateway_cost(getattr(response, "headers", None))


def _extract_usage(response: Any) -> Usage:
    """Pull token usage from an OpenAI-shaped response, defaulting to zero when
    absent (e.g. a fake client in tests)."""
    u = getattr(response, "usage", None)
    if u is None:
        return Usage()
    return Usage(
        prompt_tokens=int(getattr(u, "prompt_tokens", 0) or 0),
        completion_tokens=int(getattr(u, "completion_tokens", 0) or 0),
    )


def _tool_call_name_args(tool_call: Any) -> tuple[str, str]:
    """Tool name + raw JSON arguments from either shape the loop carries: the SDK's
    tool-call object (:meth:`SmoothAgent.run`) or the dict reassembled from stream
    deltas (:meth:`SmoothAgent.run_stream`). Lets ONE SEP plan serve both paths."""
    if isinstance(tool_call, dict):
        return tool_call["name"], tool_call.get("arguments") or ""
    return tool_call.function.name, tool_call.function.arguments


def _has_unanswered_tool_calls(messages: list[dict[str, Any]]) -> bool:
    """True when the conversation ends mid-tool-chain — an assistant message with
    ``tool_calls`` whose ``role: tool`` replies were never appended.

    Persisting that state permanently wedges the conversation: every provider
    rejects it ("an assistant message with tool_calls must be followed by tool
    messages"), and the next turn reloads the same broken checkpoint, so every
    retry fails identically. Rust never hits this because it checkpoints only at
    explicit well-formed points (``agent.rs`` ``CheckpointEvent::LlmResponse`` /
    ``ToolCallComplete``); Python persists from a ``finally``, which also runs when
    a streaming consumer abandons the generator mid-chain (``GeneratorExit``).
    """
    pending: set[str] = set()
    for m in messages:
        role = m.get("role")
        if role == "assistant":
            pending = {str(tc.get("id")) for tc in m.get("tool_calls") or []}
        elif role == "tool":
            pending.discard(str(m.get("tool_call_id")))
    return bool(pending)


async def _close_stream(stream: Any) -> None:
    """Release a model stream's underlying HTTP response.

    Leaving ``async for`` early — breaking out, or unwinding on ``GeneratorExit``
    when a WS client disconnects mid-answer — leaves the openai ``AsyncStream``'s
    httpx response open until GC. Under load that exhausts the connection pool and
    every later turn blocks acquiring one. The openai SDK spells the release
    ``close()``; a plain async generator spells it ``aclose()``. Call whichever
    exists, and swallow failures — teardown must never mask the real error.
    """
    closer = getattr(stream, "aclose", None) or getattr(stream, "close", None)
    if closer is None:
        return
    try:
        result = closer()
        if inspect.isawaitable(result):
            await result
    except Exception:  # noqa: BLE001 — a failed teardown must not replace the real error
        pass


class SmoothAgent:
    """A native, in-process agent. Construct with an OpenAI-compatible async client
    (e.g. ``openai.AsyncOpenAI(base_url=..., api_key=...)``) and :class:`AgentOptions`.
    """

    def __init__(self, chat_client: Any, options: AgentOptions) -> None:
        if chat_client is None:
            raise ValueError("chat_client is required")
        self._client = chat_client
        self._options = options
        # Extension tools join as ORDINARY tools (already namespaced
        # ``<extension>.<tool>``), so the model sees them, the loop dispatches them,
        # and the existing permission/clearance machinery gates them with no special
        # casing — the same property the Rust host has. Deferred ones go to
        # ``deferred_tools`` and stay hidden (and undispatchable) until
        # ``tool_search`` promotes them.
        if options.extensions is not None:
            options.tools = [*options.tools, *options.extensions.tools()]
            options.deferred_tools = [*options.deferred_tools, *options.extensions.deferred_tools()]
        # NOTE: deferred tools are deliberately absent from ``_tools_by_name`` — an
        # unpromoted deferred tool must resolve to nothing.
        self._tools_by_name = {t.name: t for t in options.tools}

        # Assemble the effective hook chain. When a permission mode and/or deny policy
        # is configured, a PermissionHook is prepended so it gates every call FIRST
        # (before any other hook, e.g. Narc). A deny policy with no explicit mode
        # activates the gate in BYPASS — only the built-in circuit-breakers + the
        # policy deny; everything else allowed. Neither set ⇒ the user's hooks
        # verbatim (additive no-op default).
        self._hooks: list[ToolHook] = list(options.tool_hooks)
        mode = options.permission_mode
        if mode is None and options.deny_policy is not None:
            mode = AutoMode.BYPASS
        if mode is not None:
            hook = PermissionHook(mode)
            if options.deny_policy is not None:
                hook = hook.with_deny_policy(options.deny_policy)
            self._hooks = [hook, *self._hooks]

    def _build_system(self, message: str) -> str:
        system = self._options.instructions or ""

        mem = self._options.memory
        if mem is not None:
            recalled = mem.recall(message, self._options.memory_top_k)
            if recalled:
                block = "\n".join(f"- {e.text}" for e in recalled)
                system = (
                    system + "\n\nRelevant memory (things you remember about this user/context):\n" + block
                ).strip()

        kb = self._options.knowledge
        if kb is not None:
            top_k = self._options.knowledge_top_k
            candidate_k = max(self._options.knowledge_candidate_k, top_k)
            hits = kb.query(message, candidate_k)
            hits = self._options.reranker.rerank(message, hits)[:top_k]
            if hits:
                block = "\n\n".join(f"[{h.source}] {h.content}" for h in hits)
                system = (
                    system
                    + "\n\nKnowledge base (ground all facts ONLY in this; if it is not here, say you don't know):\n"
                    + block
                ).strip()
        return system

    def _tool_specs(self, search: ToolSearch | None) -> list[dict[str, Any]] | None:
        # Eager (always-visible) tools, plus — when deferred tools exist — the
        # built-in ``tool_search`` meta-tool and any deferred tools promoted so far
        # this run. Deferred-but-unpromoted tools are deliberately omitted so the
        # model never sees their schemas until it searches for them.
        visible: list[Tool] = list(self._options.tools)
        if search is not None and search.has_deferred():
            visible.append(search)  # ToolSearch satisfies the Tool protocol (name/description/parameters)
            visible.extend(search.promoted_tools())
        if not visible:
            return None
        return [
            {
                "type": "function",
                "function": {"name": t.name, "description": t.description, "parameters": t.parameters},
            }
            for t in visible
        ]

    # ---- SEP extension seam -------------------------------------------------

    def _sep_dispatch(self, event: str, payload: dict[str, Any]) -> None:
        """Fan a turn event out to subscribed extensions. Fire and forget: observe
        events are lossy by contract and never block the turn."""
        if self._options.extensions is None:
            return
        self._options.extensions.dispatch_event(event, payload)

    def _sep_turn_complete(self, iterations: int, content: str) -> None:
        """Emit the pair of end-of-turn events in the order Rust emits them:
        ``message_end`` carrying the final assistant text, then ``turn_end``."""
        if self._options.extensions is None:
            return
        self._sep_dispatch(_SEP_MESSAGE_END, {"iteration": iterations, "content": content})
        self._sep_dispatch(_SEP_TURN_END, {"agent_id": self._options.model, "iterations": iterations})

    async def _sep_tool_call_plan(self, tool_calls: list[Any]) -> list[tuple[Any, str, str, str | None]]:
        """Fold the ``tool_call`` hook over every pending call BEFORE any of them run
        — the Python sibling of Rust's ``sep_tool_call_plan``.

        Returns one ``(tool_call, name, arguments, blocked_reason)`` per input call,
        in order: ``blocked_reason`` set means the call must not execute; otherwise
        ``arguments`` is the (possibly hook-rewritten) JSON string to run with.
        Rewrites are already scoped by the host's cross-tool guard — an extension
        may only rewrite a tool it owns, and may never redirect the call.

        Takes either tool-call shape (see :func:`_tool_call_name_args`) so both the
        streaming and non-streaming loops fold through this ONE plan, exactly as
        Rust runs it on both of its paths.

        With no host configured every call passes through untouched.
        """
        host = self._options.extensions
        if host is None:
            return [(tc, *_tool_call_name_args(tc), None) for tc in tool_calls]

        plan: list[tuple[Any, str, str, str | None]] = []
        for tc in tool_calls:
            name, raw = _tool_call_name_args(tc)
            try:
                parsed = json.loads(raw) if raw else {}
            except json.JSONDecodeError:
                parsed = {}
            folded = await host.run_tool_call_hook(name, parsed)
            if folded.blocked:
                plan.append((tc, name, raw, folded.reason or "blocked by extension"))
                continue
            # A Modify outcome replaces the whole ``{tool, arguments}`` hook input;
            # lift its ``arguments`` back out. Anything else leaves the call as-is.
            patched = raw
            if isinstance(folded.value, dict) and "arguments" in folded.value:
                patched = json.dumps(folded.value["arguments"])
            plan.append((tc, name, patched, None))
        return plan

    def _persist_turn(
        self,
        messages: list[dict[str, Any]],
        turn_messages: list[dict[str, Any]],
        thread: SmoothAgentThread | None,
    ) -> None:
        """Persist the turn on exit: checkpoint the conversation (sans system prompt,
        which is rebuilt each turn) and append this turn's new messages to the thread.

        Both loops call this from a ``finally``, so it also runs on the ABNORMAL
        exits — an exception, or a streaming consumer abandoning the generator
        part-way through a tool chain. A conversation saved from there is torn and
        unusable (see :func:`_has_unanswered_tool_calls`), so it is dropped instead:
        the store keeps the last good state and the next turn resumes from that.
        """
        if _has_unanswered_tool_calls(messages):
            return
        cp_store = self._options.checkpoint_store
        cp_id = self._options.conversation_id
        if cp_store is not None and cp_id is not None:
            cp_store.save(
                Checkpoint(conversation_id=cp_id, messages=[m for m in messages if m.get("role") != "system"])
            )
        if thread is not None:
            thread.extend(turn_messages)

    async def run(
        self,
        message: str,
        history: list[dict[str, Any]] | None = None,
        thread: SmoothAgentThread | None = None,
    ) -> AgentRunResponse:
        """Run a single turn.

        ``history`` is prior OpenAI-format messages (multi-turn). ``thread``, when
        given, is a :class:`SmoothAgentThread` carrying the conversation across runs:
        the turn is seeded from the thread's messages, and this turn's new user +
        assistant (+ tool) messages are appended back to it before returning. The
        thread takes precedence over ``history`` as the prior context.
        """
        self._sep_dispatch(_SEP_TURN_START, {"agent_id": self._options.model})
        messages: list[dict[str, Any]] = []
        system = self._build_system(message)
        if system:
            messages.append({"role": "system", "content": system})

        # Source prior conversation: the thread (if passed) wins, then the checkpoint
        # store (if configured), then the explicit ``history`` argument.
        cp_store = self._options.checkpoint_store
        cp_id = self._options.conversation_id
        prior = history
        if cp_store is not None and cp_id is not None:
            loaded = cp_store.load(cp_id)
            if loaded is not None:
                prior = loaded.messages
        if thread is not None:
            prior = list(thread.messages)
        if prior:
            messages.extend(prior)
        user_msg = {"role": "user", "content": user_content(message, self._options.next_user_images)}
        messages.append(user_msg)

        # Track this turn's new messages by identity so they can be appended back to
        # the thread on exit. Index-based slicing would be unsafe — compaction may
        # drop/reorder ``messages`` mid-turn.
        turn_messages: list[dict[str, Any]] = [user_msg]

        # Per-run promotion state for deferred tools (None when none are registered).
        search = ToolSearch(self._options.deferred_tools) if self._options.deferred_tools else None
        tool_call_count = 0
        last_text = ""
        tracker = CostTracker()

        try:
            for iteration in range(1, self._options.max_iterations + 1):
                # Keep the context window within budget before each model call.
                messages = compact(messages, self._options.max_context_tokens)
                # Recompute tool specs each iteration: a ``tool_search`` call in the
                # previous iteration may have promoted deferred tools into view.
                tool_specs = self._tool_specs(search)
                response = await self._call_model(messages, tool_specs)
                tracker.record_with_gateway_cost(
                    self._options.model,
                    _extract_usage(response),
                    _response_gateway_cost(response),
                    self._options.pricing,
                )
                choice = response.choices[0].message
                last_text = choice.content or ""

                # Append the assistant turn (OpenAI wire shape) so tool results pair to it.
                assistant_msg: dict[str, Any] = {"role": "assistant", "content": choice.content or ""}
                if choice.tool_calls:
                    assistant_msg["tool_calls"] = [
                        {
                            "id": tc.id,
                            "type": "function",
                            "function": {"name": tc.function.name, "arguments": tc.function.arguments},
                        }
                        for tc in choice.tool_calls
                    ]
                messages.append(assistant_msg)
                turn_messages.append(assistant_msg)

                # Stop early if this turn has hit its token/cost budget.
                if tracker.exceeds(self._options.budget):
                    self._sep_turn_complete(iteration, last_text)
                    return AgentRunResponse(
                        text=last_text,
                        iterations=iteration,
                        tool_calls=tool_call_count,
                        usage=tracker.usage,
                        cost_usd=tracker.cost_usd,
                        budget_exceeded=True,
                    )

                if not choice.tool_calls:
                    self._sep_turn_complete(iteration, last_text)
                    return AgentRunResponse(
                        text=last_text,
                        iterations=iteration,
                        tool_calls=tool_call_count,
                        usage=tracker.usage,
                        cost_usd=tracker.cost_usd,
                    )

                tool_call_count += len(choice.tool_calls)
                # SEP ``tool_call`` hook: folded over EVERY pending call before any of
                # them run, so an extension can veto or rewrite arguments. A vetoed
                # call never reaches _dispatch_tool; its reason becomes the tool result
                # so the model learns why. The registry's own hooks still apply after.
                plan = await self._sep_tool_call_plan(choice.tool_calls)

                async def _run_planned(name: str, args: str, blocked: str | None) -> str:
                    if blocked is not None:
                        return f"error: blocked by extension: {blocked}"
                    return await self._dispatch_tool(name, args, search)

                if self._options.parallel_tool_calls and len(plan) > 1:
                    # Dispatch all tool calls concurrently, but append the results in the
                    # original tool_calls order so the transcript stays deterministic. Each
                    # _dispatch_tool already turns failures/denials into a result string, so
                    # gather never sees an exception that would cancel its siblings.
                    results = await asyncio.gather(*(_run_planned(n, a, b) for _tc, n, a, b in plan))
                else:
                    results = [await _run_planned(n, a, b) for _tc, n, a, b in plan]
                for tc, result in zip(choice.tool_calls, results):
                    tool_msg = {"role": "tool", "tool_call_id": tc.id, "content": result}
                    messages.append(tool_msg)
                    turn_messages.append(tool_msg)

            self._sep_turn_complete(self._options.max_iterations, last_text)
            return AgentRunResponse(
                text=last_text,
                iterations=self._options.max_iterations,
                tool_calls=tool_call_count,
                usage=tracker.usage,
                cost_usd=tracker.cost_usd,
            )
        finally:
            self._persist_turn(messages, turn_messages, thread)

    async def run_stream(
        self,
        message: str,
        history: list[dict[str, Any]] | None = None,
        thread: SmoothAgentThread | None = None,
    ) -> AsyncIterator[StreamEvent]:
        """Stream a single turn, yielding incremental :data:`StreamEvent`s.

        Drives the SAME agentic loop as :meth:`run` (system/knowledge/memory build,
        seed messages, per-iteration compaction, cost tracking, budget early-stop,
        deferred-tool specs, clearance + human-gate on dispatch, checkpoint/thread
        persistence on exit) — but calls the model in STREAMING mode and emits events
        as work happens:

        * a :class:`TextEvent` per non-empty content delta as it streams in;
        * a :class:`ToolCallEvent` per requested tool call, after that iteration's
          model stream ends, BEFORE the call is dispatched;
        * a :class:`ToolResultEvent` per tool, after it finishes (in original call
          order even when ``parallel_tool_calls`` runs them concurrently);
        * exactly one terminal :class:`DoneEvent` carrying the same
          :class:`AgentRunResponse` :meth:`run` would return for the same script.

        NOTE: retry-with-backoff (``max_retries``/``retry_backoff_ms``) is intentionally
        NOT applied here — re-running the call after a mid-stream failure would re-emit
        already-yielded chunks. Retry stays scoped to non-streaming :meth:`run`; this
        mirrors the C# ``RunStreamingAsync`` decision.
        """
        messages: list[dict[str, Any]] = []
        system = self._build_system(message)
        if system:
            messages.append({"role": "system", "content": system})

        cp_store = self._options.checkpoint_store
        cp_id = self._options.conversation_id
        prior = history
        if cp_store is not None and cp_id is not None:
            loaded = cp_store.load(cp_id)
            if loaded is not None:
                prior = loaded.messages
        if thread is not None:
            prior = list(thread.messages)
        if prior:
            messages.extend(prior)
        user_msg = {"role": "user", "content": user_content(message, self._options.next_user_images)}
        messages.append(user_msg)

        turn_messages: list[dict[str, Any]] = [user_msg]
        search = ToolSearch(self._options.deferred_tools) if self._options.deferred_tools else None
        tool_call_count = 0
        last_text = ""
        tracker = CostTracker()

        try:
            for iteration in range(1, self._options.max_iterations + 1):
                messages = compact(messages, self._options.max_context_tokens)
                tool_specs = self._tool_specs(search)

                # Stream the model call, yielding text deltas while accumulating the full
                # assistant message (content + tool calls + usage).
                content = ""
                partials: dict[int, dict[str, str]] = {}
                usage: Usage = Usage()
                stream = await self._call_model_stream(messages, tool_specs)
                # Cost lives in a response HEADER. A client that returns a bare stream
                # has no response object to read one off at all, so read it from the
                # stream object when one surfaces it, and let a chunk carry it too.
                gateway_cost = _response_gateway_cost(stream)
                # The stream is closed on EVERY exit, not just exhaustion: a consumer
                # that stops iterating (WS client disconnects mid-answer) unwinds this
                # generator with GeneratorExit, and without the close the upstream
                # httpx response stays open until GC — see :func:`_close_stream`.
                try:
                    async for chunk in stream:
                        chunk_cost = _response_gateway_cost(chunk)
                        if chunk_cost is not None:
                            gateway_cost = chunk_cost
                        chunk_usage = getattr(chunk, "usage", None)
                        if chunk_usage is not None:
                            usage = Usage(
                                prompt_tokens=int(getattr(chunk_usage, "prompt_tokens", 0) or 0),
                                completion_tokens=int(getattr(chunk_usage, "completion_tokens", 0) or 0),
                            )
                        choices = getattr(chunk, "choices", None) or []
                        if not choices:
                            continue
                        delta = getattr(choices[0], "delta", None)
                        if delta is None:
                            continue
                        text_delta = getattr(delta, "content", None)
                        if text_delta:
                            content += text_delta
                            yield TextEvent(text=text_delta)
                        for tc in getattr(delta, "tool_calls", None) or []:
                            idx = int(getattr(tc, "index", 0))
                            cur = partials.setdefault(idx, {"id": "", "name": "", "arguments": ""})
                            if getattr(tc, "id", None):
                                cur["id"] = tc.id
                            fn = getattr(tc, "function", None)
                            if fn is not None:
                                if getattr(fn, "name", None):
                                    cur["name"] = fn.name
                                if getattr(fn, "arguments", None):
                                    cur["arguments"] += fn.arguments
                finally:
                    await _close_stream(stream)

                tool_calls = [partials[i] for i in sorted(partials)]
                tracker.record_with_gateway_cost(self._options.model, usage, gateway_cost, self._options.pricing)
                last_text = content

                assistant_msg: dict[str, Any] = {"role": "assistant", "content": content}
                if tool_calls:
                    assistant_msg["tool_calls"] = [
                        {
                            "id": tc["id"],
                            "type": "function",
                            "function": {"name": tc["name"], "arguments": tc["arguments"]},
                        }
                        for tc in tool_calls
                    ]
                messages.append(assistant_msg)
                turn_messages.append(assistant_msg)

                if tracker.exceeds(self._options.budget):
                    yield DoneEvent(
                        response=AgentRunResponse(
                            text=last_text,
                            iterations=iteration,
                            tool_calls=tool_call_count,
                            usage=tracker.usage,
                            cost_usd=tracker.cost_usd,
                            budget_exceeded=True,
                        )
                    )
                    return

                if not tool_calls:
                    yield DoneEvent(
                        response=AgentRunResponse(
                            text=last_text,
                            iterations=iteration,
                            tool_calls=tool_call_count,
                            usage=tracker.usage,
                            cost_usd=tracker.cost_usd,
                        )
                    )
                    return

                tool_call_count += len(tool_calls)
                # SEP ``tool_call`` hook, folded over EVERY pending call before any of
                # them run — the SAME plan ``run`` uses, and the one Rust runs on both
                # of its paths. This path is what every real UI and server drives, so
                # skipping it made an extension's Block a no-op and dropped its Modify
                # argument rewrites (redaction, scoping) silently.
                plan = await self._sep_tool_call_plan(tool_calls)

                # Emit a tool_call event per requested call (original order) BEFORE
                # dispatch, carrying the PLANNED arguments — a rewrite that redacts a
                # secret must not leak through the UI event either (Rust likewise emits
                # ToolCallStart from the planned calls).
                for _tc, name, args, _blocked in plan:
                    yield ToolCallEvent(name=name, arguments=args)

                # Reuse the SAME dispatch path as ``run`` (clearance, human-gate,
                # tool_search, JSON parsing, error-to-string, parallel_tool_calls).
                # Results surface in original call order so the stream stays deterministic.
                async def _run_planned(name: str, args: str, blocked: str | None) -> ToolResult:
                    if blocked is not None:
                        return ToolResult(content=f"error: blocked by extension: {blocked}", is_error=True)
                    return await self._dispatch_tool_result(name, args, search)

                if self._options.parallel_tool_calls and len(plan) > 1:
                    results = await asyncio.gather(*(_run_planned(n, a, b) for _tc, n, a, b in plan))
                else:
                    results = [await _run_planned(n, a, b) for _tc, n, a, b in plan]
                for tc, result in zip(tool_calls, results):
                    tool_msg = {"role": "tool", "tool_call_id": tc["id"], "content": result.content}
                    messages.append(tool_msg)
                    turn_messages.append(tool_msg)
                    yield ToolResultEvent(name=tc["name"], result=result.content, details=result.details)

            yield DoneEvent(
                response=AgentRunResponse(
                    text=last_text,
                    iterations=self._options.max_iterations,
                    tool_calls=tool_call_count,
                    usage=tracker.usage,
                    cost_usd=tracker.cost_usd,
                )
            )
        finally:
            # Also runs on GeneratorExit — a consumer that stops iterating mid-tool-chain
            # must NOT leave a torn conversation behind; ``_persist_turn`` drops it.
            self._persist_turn(messages, turn_messages, thread)

    def _mark_prompt_cache(self, messages: list[dict[str, Any]], tool_specs: list[dict[str, Any]] | None) -> None:
        """Stamp Anthropic prompt-cache markers, when the upstream understands them.

        A no-op for every other route, so the request stays wire-identical on the
        OpenAI/Gemini/Groq paths and under the mock provider (which reports no base
        url). ``api_base_url`` is the seam :class:`~.gateway_client.GatewayLlmProvider`
        populates from the SDK.
        """
        if supports_anthropic_cache_control(self._options.model, getattr(self._client, "api_base_url", None)):
            apply_cache_control(messages, tool_specs)

    async def _call_model_stream(
        self, messages: list[dict[str, Any]], tool_specs: list[dict[str, Any]] | None
    ) -> AsyncIterator[Any]:
        """Open a streaming model call, returning the async iterator of chunks.

        Production wires this to the real ``openai`` SDK's
        ``chat.completions.create(..., stream=True)`` (which returns an async stream
        of OpenAI chunk objects). The seam exists so the mock + loop are testable
        without a live model. Retry is deliberately not applied here — see
        :meth:`run_stream`.
        """
        self._mark_prompt_cache(messages, tool_specs)
        return await self._client.chat.completions.create(
            model=self._options.model,
            messages=messages,
            tools=tool_specs,
            temperature=self._options.temperature,
            max_tokens=effective_max_tokens(self._options.max_tokens, self._options.model_max_output),
            stream=True,
            # Empty/None metadata sends nothing — wire-identical to unset (Rust parity).
            **({"metadata": self._options.metadata} if self._options.metadata else {}),
        )

    async def _call_model(self, messages: list[dict[str, Any]], tool_specs: list[dict[str, Any]] | None) -> Any:
        """Invoke the model with bounded retry-with-exponential-backoff.

        On a transient error (anything the client raises — rate-limit, 5xx, dropped
        connection) the call is retried up to ``max_retries`` additional times, waiting
        ``retry_backoff_ms * 2 ** (n - 1)`` ms before the n-th (1-indexed) retry. If all
        attempts fail the LAST error propagates, so the turn fails exactly as it did
        before retries existed. Only this model call is retried — tool execution is not.
        """
        attempt = 0
        self._mark_prompt_cache(messages, tool_specs)
        while True:
            try:
                return await self._client.chat.completions.create(
                    model=self._options.model,
                    messages=messages,
                    tools=tool_specs,
                    temperature=self._options.temperature,
                    max_tokens=effective_max_tokens(self._options.max_tokens, self._options.model_max_output),
                    # Empty/None metadata sends nothing — wire-identical to unset (Rust parity).
                    **({"metadata": self._options.metadata} if self._options.metadata else {}),
                )
            except Exception:
                if attempt >= self._options.max_retries:
                    raise  # retries exhausted (or disabled): propagate the last error
                attempt += 1
                delay_ms = self._options.retry_backoff_ms * (2 ** (attempt - 1))
                if delay_ms > 0:
                    await asyncio.sleep(delay_ms / 1000)

    async def _dispatch_tool(self, name: str, raw_arguments: str, search: ToolSearch | None) -> str:
        return (await self._dispatch_tool_result(name, raw_arguments, search)).content

    async def _dispatch_tool_result(self, name: str, raw_arguments: str, search: ToolSearch | None) -> ToolResult:
        """``_dispatch_tool`` returning the full :class:`ToolResult`, so callers
        that surface results to a UI (:meth:`run_stream`) can forward the
        structured ``details`` a ``post_call`` hook attached — the model itself
        only ever sees ``content``. Mirrors the Rust engine's
        ``AgentEvent::ToolCallComplete.details``."""
        import json

        def err_result(content: str) -> ToolResult:
            return ToolResult(content=content, is_error=True)

        # Enforce the role's tool clearance before dispatch: a forbidden tool is
        # never executed — the model is told it isn't permitted, mirroring how the
        # loop surfaces other tool errors.
        clearance = self._options.clearance
        if clearance is not None and not clearance.is_allowed(name):
            return err_result(f"error: tool '{name}' is not permitted for this role")

        # Resolve the tool: eager tools first, then the built-in ``tool_search``
        # meta-tool, then deferred tools that have been promoted. An unpromoted
        # deferred tool resolves to nothing — it's invisible until searched for.
        tool: Tool | None = self._tools_by_name.get(name)
        if tool is None and search is not None:
            if name == search.name:
                tool = search
            else:
                tool = search.tool_by_name(name)
        if tool is None:
            return err_result(f"error: unknown tool '{name}'")
        try:
            args = json.loads(raw_arguments) if raw_arguments else {}
        except json.JSONDecodeError:
            return err_result(f"error: tool '{name}' received invalid JSON arguments")

        # Human-in-the-loop: pause for approval before running a flagged (write/sensitive)
        # tool. A denial is fed back to the model as a result — the tool never runs.
        gate = self._options.human_gate
        needs_approval = self._options.requires_approval
        if gate is not None and needs_approval is not None and needs_approval(name, args):
            request = HumanApprovalRequest(tool_name=name, arguments=args, prompt=f"Approve calling tool '{name}'?")
            decision = await gate.request_approval(request)
            if not decision.is_approved:
                return err_result(f"Denied by human: {decision.reason or 'no reason given'}")

        # Tool-call lifecycle hooks (parity with the Rust ``ToolHook`` trait). The
        # effective chain (built in __init__) prepends a PermissionHook when a
        # permission mode / deny policy is configured, so it gates FIRST. An empty
        # chain (the default) makes both loops no-ops → behavior unchanged.
        hooks = self._hooks
        call = ToolCall(name=name, arguments=args)

        # pre_call: any hook may block the call by raising. The message is fed back
        # to the model as the tool result; the tool never runs. Mirrors the Rust
        # engine returning a "blocked by hook" result on a pre_call Err.
        for hook in hooks:
            try:
                await hook.pre_call(call)
            except Exception as exc:  # noqa: BLE001 — a blocking hook informs the model, it doesn't crash the turn
                return err_result(f"blocked by hook: {exc}")

        try:
            result = ToolResult(content=await tool.execute(call.arguments), is_error=False)
        except Exception as exc:  # noqa: BLE001 — surface tool failures to the model, don't crash the turn
            result = ToolResult(content=f"error: tool '{name}' failed: {exc}", is_error=True)

        # post_call: hooks may redact ``result.content`` in place (the mutable seam).
        # A hook raising here is swallowed — the possibly-redacted result still
        # reaches the caller; a broken hook must never crash the turn.
        for hook in hooks:
            try:
                await hook.post_call(call, result)
            except Exception:  # noqa: BLE001 — post-hook failure must not surface; result still returns
                pass

        return result


def delegate_tool(name: str, description: str, child: SmoothAgent, task_property: str = "task") -> FunctionTool:
    """Build a :class:`Tool` that delegates a subtask to a child :class:`SmoothAgent`.

    A sub-agent is just a tool backed by another agent: the model calls this tool
    with a ``task`` argument, the child agent runs that task, and the child's final
    reply becomes the tool result — composing with the existing tool loop, no
    special wiring. The child can have its own instructions, tools, knowledge, etc.
    """

    async def _run(args: dict[str, Any]) -> str:
        task = str(args.get(task_property, ""))
        result = await child.run(task)
        return result.text

    return FunctionTool(
        name=name,
        description=description,
        parameters={
            "type": "object",
            "properties": {
                task_property: {"type": "string", "description": "The subtask for the sub-agent to perform."}
            },
            "required": [task_property],
        },
        func=_run,
    )
