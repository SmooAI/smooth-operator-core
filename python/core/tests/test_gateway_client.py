"""The real HTTP client, against a real local gateway.

Every assertion here is a round-trip: an ``http.server`` speaking the OpenAI
``/chat/completions`` shape (JSON and SSE), driven through
:class:`GatewayLlmProvider` and a live :class:`SmoothAgent`. Nothing is mocked below
the socket, so these cover the things a mock cannot: that the SSE framing parses,
that ``metadata`` reaches the wire (and is ABSENT when unset), and above all that
the cost header is read on the streaming path. The response headers survive the body
being consumed just fine; the regression core#102 fixed in Rust was keeping only the
stream and dropping the response, leaving nothing to read a header off at all.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from typing import Any

import pytest

from smooth_operator_core import AgentOptions, GatewayLlmProvider, ModelPricing, SmoothAgent
from smooth_operator_core.agent import DoneEvent, TextEvent

# $1000/1M tokens makes the local estimate large and obviously distinct from any
# gateway-reported cost, so "which number won" is never ambiguous.
PRICING = {"m": ModelPricing(input_per_mtok=1000.0, output_per_mtok=1000.0)}


class _Gateway:
    """A local OpenAI-compatible endpoint, scripted per test.

    ``received`` collects every request body so the wire itself can be asserted on.
    """

    def __init__(
        self,
        *,
        headers: dict[str, str] | None = None,
        text: str = "",
        usage: dict[str, int] | None = None,
        deltas: list[str] | None = None,
    ) -> None:
        self.headers = headers or {}
        self.text = text
        self.usage = usage
        self.deltas = deltas or []
        self.received: list[dict[str, Any]] = []

        script = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_args: Any) -> None:  # keep pytest output clean
                pass

            def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler's naming
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                script.received.append(body)

                streaming = bool(body.get("stream"))
                self.send_response(200)
                for name, value in script.headers.items():
                    self.send_header(name, value)
                self.send_header("Content-Type", "text/event-stream" if streaming else "application/json")
                self.end_headers()

                if streaming:
                    for delta in script.deltas:
                        self._sse({"choices": [{"index": 0, "delta": {"content": delta}}]})
                    if script.usage:
                        self._sse({"choices": [], "usage": script.usage})
                    self.wfile.write(b"data: [DONE]\n\n")
                    return

                self.wfile.write(
                    json.dumps(
                        {
                            "model": "m",
                            "choices": [{"index": 0, "message": {"role": "assistant", "content": script.text}}],
                            "usage": script.usage,
                        }
                    ).encode()
                )

            def _sse(self, payload: dict[str, Any]) -> None:
                self.wfile.write(f"data: {json.dumps(payload)}\n\n".encode())

        self._server = HTTPServer(("127.0.0.1", 0), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)

    def __enter__(self) -> "_Gateway":
        self._thread.start()
        return self

    def __exit__(self, *_exc: Any) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=5)

    @property
    def base_url(self) -> str:
        host, port = self._server.server_address[:2]
        return f"http://{host}:{port}/v1"

    def agent(self, **option_overrides: Any) -> SmoothAgent:
        provider = GatewayLlmProvider(base_url=self.base_url, api_key="k")
        return SmoothAgent(provider, AgentOptions(model="m", pricing=PRICING, **option_overrides))


async def _drain(agent: SmoothAgent, message: str = "hi") -> tuple[list[str], Any]:
    """Run a streaming turn, returning its text deltas and the terminal response."""
    texts: list[str] = []
    final = None
    async for event in agent.run_stream(message):
        if isinstance(event, TextEvent):
            texts.append(event.text)
        elif isinstance(event, DoneEvent):
            final = event.response
    return texts, final


# ---- non-streaming ----


async def test_round_trips_content_and_usage() -> None:
    with _Gateway(text="hello from the gateway", usage={"prompt_tokens": 11, "completion_tokens": 7}) as gw:
        result = await gw.agent().run("hi")

    assert result.text == "hello from the gateway"
    assert result.usage.prompt_tokens == 11
    assert result.usage.completion_tokens == 7
    assert gw.received[0]["model"] == "m"
    assert gw.received[0]["messages"] == [{"role": "user", "content": "hi"}]


async def test_folds_the_gateway_cost_header_into_the_turn_cost() -> None:
    with _Gateway(
        text="hi",
        usage={"prompt_tokens": 10, "completion_tokens": 5},
        headers={"x-litellm-response-cost-margin-amount": "0.25"},
    ) as gw:
        result = await gw.agent().run("hi")

    # The gateway's number wins outright — not summed with, not shadowed by, the
    # $0.015 local estimate for these 15 tokens.
    assert result.cost_usd == 0.25


async def test_falls_back_to_local_pricing_when_no_cost_header() -> None:
    with _Gateway(text="hi", usage={"prompt_tokens": 10, "completion_tokens": 5}) as gw:
        result = await gw.agent().run("hi")

    # Unmeasured must stay unmeasured: an estimate, never a recorded $0.
    assert result.cost_usd == pytest.approx(0.015)


async def test_zero_margin_does_not_zero_real_spend() -> None:
    with _Gateway(
        text="hi",
        usage={"prompt_tokens": 10, "completion_tokens": 5},
        headers={"x-litellm-response-cost-margin-amount": "0", "x-litellm-response-cost-original": "0.5"},
    ) as gw:
        result = await gw.agent().run("hi")

    assert result.cost_usd == 0.5


# ---- streaming ----


async def test_streams_sse_deltas_and_assembles_the_text() -> None:
    with _Gateway(deltas=["Hel", "lo ", "world"], usage={"prompt_tokens": 4, "completion_tokens": 3}) as gw:
        texts, final = await _drain(gw.agent())

    assert texts == ["Hel", "lo ", "world"]
    assert final.text == "Hello world"
    assert final.usage.prompt_tokens == 4
    assert final.usage.completion_tokens == 3
    assert gw.received[0]["stream"] is True


async def test_reads_the_cost_header_before_the_sse_body() -> None:
    with _Gateway(
        deltas=["hi"],
        usage={"prompt_tokens": 10, "completion_tokens": 5},
        headers={"x-litellm-response-cost": "0.75"},
    ) as gw:
        _texts, final = await _drain(gw.agent())

    # This is the whole point of the streaming path: the headers are unreachable
    # once the stream has been scanned, so a $0.75 here proves they were read first.
    assert final.cost_usd == 0.75


async def test_streaming_falls_back_to_local_pricing_without_a_cost_header() -> None:
    with _Gateway(deltas=["hi"], usage={"prompt_tokens": 10, "completion_tokens": 5}) as gw:
        _texts, final = await _drain(gw.agent())

    assert final.cost_usd == pytest.approx(0.015)


# ---- request metadata (core#100) ----


async def test_sends_metadata_on_the_wire_when_set() -> None:
    with _Gateway(text="hi") as gw:
        await gw.agent(metadata={"agent_slug": "support"}).run("hi")

    assert gw.received[0]["metadata"] == {"agent_slug": "support"}


async def test_sends_metadata_on_the_streaming_wire_too() -> None:
    with _Gateway(deltas=["hi"]) as gw:
        await _drain(gw.agent(metadata={"agent_slug": "support"}))

    assert gw.received[0]["metadata"] == {"agent_slug": "support"}


async def test_omits_metadata_entirely_when_unset() -> None:
    with _Gateway(text="hi") as gw:
        await gw.agent().run("hi")

    assert "metadata" not in gw.received[0]


async def test_omits_null_tools_when_a_turn_has_none() -> None:
    # The agent passes tools=None for a toolless turn. A literal "tools": null is not
    # what unset means on the wire, and real gateways reject it.
    with _Gateway(text="hi") as gw:
        await gw.agent().run("hi")

    assert "tools" not in gw.received[0]


def test_requires_a_client_or_both_base_url_and_key() -> None:
    with pytest.raises(ValueError):
        GatewayLlmProvider(base_url="http://x/v1")
    with pytest.raises(ValueError):
        GatewayLlmProvider(api_key="k")
