"""An ``LlmProvider`` seam over the LLM call so the agentic loop can be unit-tested
deterministically, without a live model or network.

The agent already takes an injected OpenAI-compatible chat client (the ``openai``
SDK pointed at a gateway). This module *formalizes* that seam as a
:class:`LlmProvider` Protocol — any object exposing the duck-typed
``chat.completions.create(...)`` surface already satisfies it, so the existing
``SmoothAgent`` constructor is unchanged and backward compatible.

It also ships a reusable, exported :class:`MockLlmProvider` that replaces the
ad-hoc fake clients the tests rolled by hand. The mock:

* is constructed with a script of responses — plain text, tool-call responses,
  and errors;
* returns them in FIFO order across calls;
* records each request (the messages + tool specs it was given) so a test can
  assert on what the agent sent.

This mirrors the BEHAVIOR of the Rust reference's ``MockLlmClient``
(``rust/smooth-operator-core/src/llm_provider.rs``). The mock implements both the
non-streaming call (``create(...)``, used by :meth:`SmoothAgent.run`) and the
streaming call (``create(..., stream=True)``, used by :meth:`SmoothAgent.run_stream`):
it replays the SAME FIFO script as chunked deltas — text split into a few pieces,
tool-call ``arguments`` split across two chunks to exercise the accumulator, and a
final chunk carrying usage. Structured-output lands when that feature lands here.
"""

from __future__ import annotations

from types import SimpleNamespace
from typing import Any, AsyncIterator, Protocol, runtime_checkable


@runtime_checkable
class LlmProvider(Protocol):
    """The LLM call surface the agent loop depends on.

    This is exactly the slice of the ``openai`` async client the agent uses:
    ``provider.chat.completions.create(model=..., messages=..., tools=..., ...)``
    returning an OpenAI-shaped response (``.choices[0].message`` with
    ``.content`` / ``.tool_calls``, and an optional ``.usage``).

    Production wires the real ``openai.AsyncOpenAI`` client (which satisfies this
    structurally); tests inject a :class:`MockLlmProvider`.
    """

    chat: Any


# ── response builders (handy for scripting the mock and for assertions) ──────


def usage(prompt_tokens: int = 0, completion_tokens: int = 0) -> SimpleNamespace:
    """An OpenAI-shaped ``usage`` object to attach to a scripted response."""
    return SimpleNamespace(prompt_tokens=prompt_tokens, completion_tokens=completion_tokens)


def text_response(content: str, usage: SimpleNamespace | None = None) -> SimpleNamespace:
    """An OpenAI-shaped assistant message that is plain text (no tool calls).

    An optional ``usage`` rides along (used by the streaming path's final chunk and
    the non-streaming response's ``.usage``).
    """
    return SimpleNamespace(content=content, tool_calls=None, usage=usage)


def tool_call_response(
    call_id: str, name: str, arguments: str, usage: SimpleNamespace | None = None
) -> SimpleNamespace:
    """An OpenAI-shaped assistant message that requests a single tool call.

    ``arguments`` is the raw JSON-string the model emits for the call's arguments
    (mirroring the wire shape the agent parses).
    """
    tool_call = SimpleNamespace(id=call_id, function=SimpleNamespace(name=name, arguments=arguments))
    return SimpleNamespace(content=None, tool_calls=[tool_call], usage=usage)


class RecordedCall:
    """One request the mock received, captured for assertions.

    ``messages`` and ``tools`` are the exact kwargs the agent passed to
    ``chat.completions.create`` for this call.
    """

    __slots__ = ("messages", "tools", "kwargs")

    def __init__(self, kwargs: dict[str, Any]) -> None:
        self.kwargs = kwargs
        self.messages: list[dict[str, Any]] = list(kwargs.get("messages") or [])
        self.tools: list[dict[str, Any]] | None = kwargs.get("tools")

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        return f"RecordedCall(messages={self.messages!r}, tools={self.tools!r})"


class _ScriptedError(Exception):
    """Marker for an error the script wants raised from a chat call."""


def _split_into_chunks(s: str, n: int = 3) -> list[str]:
    """Split ``s`` into up to ``n`` roughly-equal non-empty pieces."""
    if not s:
        return []
    parts = min(n, max(1, len(s)))
    size = -(-len(s) // parts)  # ceil division
    return [s[i : i + size] for i in range(0, len(s), size)]


async def _stream_chunks(message: SimpleNamespace) -> AsyncIterator[SimpleNamespace]:
    """Yield OpenAI-shaped streaming chunks for a scripted ``message``.

    Text is split into a few content-delta chunks; each tool call is emitted as an
    opening chunk (id + name + first half of arguments) plus a second chunk with the
    rest of the arguments (exercising the agent's index-keyed accumulator); a final
    empty-delta chunk carries usage.
    """
    content = message.content or ""
    for piece in _split_into_chunks(content):
        if piece:
            yield SimpleNamespace(
                choices=[SimpleNamespace(delta=SimpleNamespace(content=piece, tool_calls=None))], usage=None
            )
    for index, tc in enumerate(message.tool_calls or []):
        args = tc.function.arguments or ""
        mid = len(args) // 2
        open_tc = SimpleNamespace(
            index=index, id=tc.id, function=SimpleNamespace(name=tc.function.name, arguments=args[:mid])
        )
        yield SimpleNamespace(
            choices=[SimpleNamespace(delta=SimpleNamespace(content=None, tool_calls=[open_tc]))], usage=None
        )
        rest_tc = SimpleNamespace(index=index, id=None, function=SimpleNamespace(name=None, arguments=args[mid:]))
        yield SimpleNamespace(
            choices=[SimpleNamespace(delta=SimpleNamespace(content=None, tool_calls=[rest_tc]))], usage=None
        )
    yield SimpleNamespace(
        choices=[SimpleNamespace(delta=SimpleNamespace(content=None, tool_calls=None))],
        usage=getattr(message, "usage", None),
    )


class _Completions:
    """Implements the ``chat.completions`` surface: replay + record."""

    def __init__(self, owner: MockLlmProvider) -> None:
        self._owner = owner

    async def create(self, **kwargs: Any) -> Any:
        # ``stream=True`` returns an async iterator of chunks; otherwise a full response.
        streaming = bool(kwargs.pop("stream", False))
        self._owner._calls.append(RecordedCall(kwargs))
        if not self._owner._script:
            message: Any = text_response("")
        else:
            message = self._owner._script.pop(0)
        if isinstance(message, _ScriptedError):
            if streaming:
                # The error surfaces when the stream is first iterated.
                async def _erroring() -> AsyncIterator[SimpleNamespace]:
                    raise message
                    yield  # pragma: no cover - unreachable, makes this an async generator

                return _erroring()
            raise message
        if streaming:
            return _stream_chunks(message)
        return SimpleNamespace(choices=[SimpleNamespace(message=message)], usage=getattr(message, "usage", None))


class MockLlmProvider:
    """A deterministic :class:`LlmProvider` for tests.

    Script the responses it should return (FIFO), drive your code, then assert on
    :attr:`calls`. Construct with an optional list of scripted outcomes, or build
    it up fluently with :meth:`push_text` / :meth:`push_tool_call` /
    :meth:`push_error`.

    Example::

        mock = MockLlmProvider()
        mock.push_text("hello there")
        agent = SmoothAgent(mock, AgentOptions())
        result = await agent.run("hi")
        assert result.text == "hello there"
        assert mock.call_count == 1
        assert mock.calls[0].messages[-1]["content"] == "hi"
    """

    def __init__(self, script: list[Any] | None = None) -> None:
        self._script: list[Any] = list(script or [])
        self._calls: list[RecordedCall] = []
        self.chat = SimpleNamespace(completions=_Completions(self))

    # ── scripting (fluent: each returns self) ────────────────────────────────

    def push_response(self, message: Any) -> MockLlmProvider:
        """Queue a raw OpenAI-shaped assistant message for the next call."""
        self._script.append(message)
        return self

    def push_text(self, content: str, usage: SimpleNamespace | None = None) -> MockLlmProvider:
        """Queue a plain-text response (with optional usage) for the next call."""
        return self.push_response(text_response(content, usage))

    def push_tool_call(
        self, call_id: str, name: str, arguments: str, usage: SimpleNamespace | None = None
    ) -> MockLlmProvider:
        """Queue a single-tool-call response (with optional usage) for the next call."""
        return self.push_response(tool_call_response(call_id, name, arguments, usage))

    def push_error(self, message: str) -> MockLlmProvider:
        """Queue an error to be raised on the next call."""
        self._script.append(_ScriptedError(message))
        return self

    # ── recordings ───────────────────────────────────────────────────────────

    @property
    def calls(self) -> list[RecordedCall]:
        """Every request the mock has received so far, in order."""
        return self._calls

    @property
    def call_count(self) -> int:
        """Number of requests received."""
        return len(self._calls)

    @property
    def last_call(self) -> RecordedCall | None:
        """The most recent request, if any."""
        return self._calls[-1] if self._calls else None
