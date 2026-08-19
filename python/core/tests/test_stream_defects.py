"""Regression tests for three defects that lived ONLY on the streaming path.

``run_stream`` is what every real UI and server drives, and ``run`` was correct in
all three cases — which is exactly why these were invisible. Each test below fails
against the pre-fix engine:

1. the SEP ``tool_call`` hook was never folded in, so an extension's veto was
   ignored and its argument rewrites dropped;
2. the model stream was never closed when the consumer stopped iterating, leaking
   the upstream HTTP response until GC;
3. the ``finally`` checkpointed on ``GeneratorExit`` too, persisting an assistant
   ``tool_calls`` message with no tool results — a conversation every provider
   rejects, reloaded identically by every later turn.
"""

from __future__ import annotations

import json
from types import SimpleNamespace
from typing import Any

import pytest
from test_extension_agent_wiring import FakeHost, _recording_tool

from smooth_operator_core import (
    AgentOptions,
    InMemoryCheckpointStore,
    MockLlmProvider,
    SmoothAgent,
    TextEvent,
    ToolCallEvent,
    ToolResultEvent,
)


def _provider_would_reject(messages: list[dict[str, Any]]) -> bool:
    """The upstream's rule, spelled out here independently of the engine's own
    helper: an assistant message carrying ``tool_calls`` must be followed by tool
    messages, or the provider 400s."""
    for i, m in enumerate(messages):
        if m.get("role") == "assistant" and m.get("tool_calls"):
            nxt = messages[i + 1] if i + 1 < len(messages) else None
            if nxt is None or nxt.get("role") != "tool":
                return True
    return False


# ── 1. the SEP tool_call hook must run on the streaming path ─────────────────


@pytest.mark.asyncio
async def test_stream_honors_extension_veto():
    """An extension that Blocks a tool is honored non-streaming; it must be honored
    here too, or a `Block` on `payments.refund` is worthless where it matters."""
    refund, seen = _recording_tool("payments.refund")
    host = FakeHost(block_tool="payments.refund", block_reason="policy: no refunds")
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "payments.refund", json.dumps({"amount": 100})).push_text("understood")

    agent = SmoothAgent(mock, AgentOptions(tools=[refund], extensions=host))
    events = [e async for e in agent.run_stream("refund it")]

    assert seen["calls"] == 0, "a vetoed tool call must never execute on the streaming path"
    result = next(e for e in events if isinstance(e, ToolResultEvent))
    assert "policy: no refunds" in result.result, "the model must be told why the call was blocked"


@pytest.mark.asyncio
async def test_stream_applies_extension_argument_rewrite():
    """A Modify rewrite (redaction, scoping) has to reach the tool AND the emitted
    event — a redacted argument leaking through the UI event is the same failure."""
    tool, seen = _recording_tool("weather.forecast")
    host = FakeHost(tools=[tool], patch_tool="weather.forecast", patch_arguments={"city": "Boston"})
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "weather.forecast", json.dumps({"city": "NYC"})).push_text("done")

    agent = SmoothAgent(mock, AgentOptions(extensions=host))
    events = [e async for e in agent.run_stream("weather?")]

    assert seen["args"]["city"] == "Boston", "arguments not rewritten by the hook"
    call_event = next(e for e in events if isinstance(e, ToolCallEvent))
    assert json.loads(call_event.arguments) == {"city": "Boston"}, "the event leaked the un-rewritten arguments"


# ── 2. the model stream must be released when the consumer walks away ────────


class _ClosableStream:
    """The openai ``AsyncStream`` shape: async-iterable, released via ``close()``
    (async generators spell the same thing ``aclose()``)."""

    def __init__(self, chunks: list[Any]) -> None:
        self._chunks = chunks
        self.closed = False

    def __aiter__(self) -> Any:
        return self._iterate()

    async def _iterate(self) -> Any:
        for chunk in self._chunks:
            yield chunk

    async def close(self) -> None:
        self.closed = True


def _text_chunk(text: str) -> SimpleNamespace:
    return SimpleNamespace(choices=[SimpleNamespace(delta=SimpleNamespace(content=text, tool_calls=None))], usage=None)


class _StreamingClient:
    """A chat client that hands back one :class:`_ClosableStream` per call."""

    def __init__(self, stream: _ClosableStream) -> None:
        self.stream = stream
        self.chat = SimpleNamespace(completions=SimpleNamespace(create=self._create))

    async def _create(self, **_kwargs: Any) -> _ClosableStream:
        return self.stream


@pytest.mark.asyncio
async def test_abandoned_stream_is_closed():
    """A WS client disconnecting mid-answer abandons the generator. Without an
    explicit close the httpx response stays open until GC, and under load the
    connection pool exhausts."""
    stream = _ClosableStream([_text_chunk("hel"), _text_chunk("lo"), _text_chunk(" there")])
    agent = SmoothAgent(_StreamingClient(stream), AgentOptions())

    events = agent.run_stream("hi")
    first = await events.__anext__()
    assert isinstance(first, TextEvent)
    await events.aclose()  # the consumer walks away mid-answer

    assert stream.closed, "the model stream was abandoned without being closed"


# ── 3. an abandoned turn must not checkpoint a torn conversation ─────────────


@pytest.mark.asyncio
async def test_abandoned_mid_tool_chain_does_not_wedge_the_conversation():
    """Abandoning between the tool call and its result used to persist an assistant
    ``tool_calls`` message with no tool replies. Every later turn reloaded it and
    the provider 400'd on all of them — a permanently wedged conversation."""
    tool, _ = _recording_tool("lookup")
    store = InMemoryCheckpointStore()
    opts = AgentOptions(tools=[tool], checkpoint_store=store, conversation_id="conv-1")

    mock = MockLlmProvider()
    mock.push_tool_call("c1", "lookup", "{}").push_text("found it")
    events = SmoothAgent(mock, opts).run_stream("look it up")
    async for event in events:
        if isinstance(event, ToolCallEvent):
            break  # abandon after the call is announced, before its result exists
    await events.aclose()

    saved = store.load("conv-1")
    assert saved is None or not _provider_would_reject(saved.messages), "a torn conversation was checkpointed"

    # The load-bearing assertion: the NEXT turn still works.
    resumed = MockLlmProvider().push_text("fresh answer")
    result = await SmoothAgent(resumed, AgentOptions(tools=[tool], **_cp(store))).run("still there?")
    assert result.text == "fresh answer"
    assert not _provider_would_reject(resumed.calls[0].messages), "the wedged checkpoint was replayed upstream"


def _cp(store: InMemoryCheckpointStore) -> dict[str, Any]:
    return {"checkpoint_store": store, "conversation_id": "conv-1"}


@pytest.mark.asyncio
async def test_completed_stream_still_checkpoints():
    """The guard must not cost the normal case its checkpoint."""
    tool, _ = _recording_tool("lookup")
    store = InMemoryCheckpointStore()
    mock = MockLlmProvider()
    mock.push_tool_call("c1", "lookup", "{}").push_text("found it")

    agent = SmoothAgent(mock, AgentOptions(tools=[tool], **_cp(store)))
    async for _event in agent.run_stream("look it up"):
        pass

    saved = store.load("conv-1")
    assert saved is not None
    assert [m["role"] for m in saved.messages] == ["user", "assistant", "tool", "assistant"]
