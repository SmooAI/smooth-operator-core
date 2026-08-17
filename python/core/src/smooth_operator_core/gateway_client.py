"""A REAL OpenAI-compatible chat client for the agent's :class:`LlmProvider` seam.

Until now the Python engine could only be driven by :class:`MockLlmProvider` or a
client the consumer hand-rolled; this is the shipped one, pointed at any
``/chat/completions`` endpoint (the SmooAI gateway, or anything OpenAI-compatible)
with a base URL + API key.

It is a thin adapter over the ``openai`` SDK, which this package already depends on
— the SDK owns the wire format, the SSE framing, timeouts and connection retries, so
the only thing this module adds is the bit the SDK's parsed response throws away:

**The gateway cost header.** Cost is reported ONLY in a response header, and on the
streaming path those headers are gone once the SSE body is being consumed — the
regression core#102 fixed in Rust. ``with_raw_response`` hands back the headers
alongside the (not yet consumed) payload, so both paths read them BEFORE touching
the body and expose them on the documented seam
(:func:`smooth_operator_core.agent._response_gateway_cost` reads ``.headers``).

The request body is otherwise passed through verbatim, so the agent's ``metadata``
(core#100) and every other field reach the wire unchanged. ``None`` kwargs are
dropped rather than serialized: the agent passes ``tools=None`` when a turn has no
tools, and a literal ``"tools": null`` is not what "unset" means on the wire.

:class:`MockLlmProvider` remains the default/test seam. This is opt-in — nothing
constructs it for you, so an unwired consumer behaves exactly as before::

    agent = SmoothAgent(GatewayLlmProvider(base_url="https://llm.smoo.ai/v1", api_key=key), options)
"""

from __future__ import annotations

import inspect
from typing import Any

from openai import AsyncOpenAI

__all__ = ["GatewayLlmProvider"]


class _WithHeaders:
    """Delegates every attribute to ``wrapped``, plus a real ``.headers``.

    The engine's cost seam looks for ``.headers`` on the response (and on a stream)
    — neither the SDK's parsed ``ChatCompletion`` (a frozen pydantic model) nor its
    ``AsyncStream`` carries them, so this wrapper is what puts the two together.
    """

    def __init__(self, wrapped: Any, headers: Any) -> None:
        self._wrapped = wrapped
        self.headers = headers

    def __getattr__(self, name: str) -> Any:
        return getattr(self._wrapped, name)

    def __aiter__(self) -> Any:
        # Streaming responses are async-iterated by the agent; __getattr__ is not
        # consulted for dunder lookups, so this has to be spelled out.
        return self._wrapped.__aiter__()


class _Completions:
    """Implements the ``chat.completions.create`` surface the agent depends on."""

    def __init__(self, client: AsyncOpenAI) -> None:
        self._client = client

    async def create(self, **kwargs: Any) -> Any:
        # ``tools=None`` means "no tools this turn", not "send a null" — dropping
        # Nones keeps the wire byte-identical to an unset field (Rust parity, and
        # the same reason the agent omits empty metadata entirely).
        body = {k: v for k, v in kwargs.items() if v is not None}
        # with_raw_response resolves once the response HEAD is in, so the cost header
        # is read while the SSE body is still untouched. Reading it after iterating
        # the stream would find nothing — the exact bug core#102 fixed in Rust.
        raw = await self._client.chat.completions.with_raw_response.create(**body)
        parsed = raw.parse()
        if inspect.isawaitable(parsed):
            parsed = await parsed
        return _WithHeaders(parsed, raw.headers)


class GatewayLlmProvider:
    """An :class:`LlmProvider` backed by a live OpenAI-compatible endpoint.

    Satisfies the seam structurally (it exposes ``chat.completions.create``), so it
    drops straight into :class:`SmoothAgent` wherever a :class:`MockLlmProvider`
    would go. Pass ``client`` to bring your own configured ``AsyncOpenAI`` (custom
    http client, proxy, default headers); otherwise one is built from ``base_url``
    and ``api_key``.
    """

    def __init__(
        self,
        base_url: str | None = None,
        api_key: str | None = None,
        *,
        client: AsyncOpenAI | None = None,
        timeout: float | None = None,
        max_retries: int | None = None,
    ) -> None:
        if client is None:
            if not base_url or not api_key:
                raise ValueError("GatewayLlmProvider needs either a client, or both base_url and api_key")
            extra: dict[str, Any] = {}
            if timeout is not None:
                extra["timeout"] = timeout
            if max_retries is not None:
                extra["max_retries"] = max_retries
            client = AsyncOpenAI(base_url=base_url, api_key=api_key, **extra)
        self._client = client
        self.chat = _Chat(client)


class _Chat:
    """The ``.chat`` namespace, so ``provider.chat.completions.create`` resolves."""

    def __init__(self, client: AsyncOpenAI) -> None:
        self.completions = _Completions(client)
