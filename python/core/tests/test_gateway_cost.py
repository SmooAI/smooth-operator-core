"""The gateway reports per-request cost ONLY in a response header.

The Python engine takes an injected OpenAI-compatible client, so it has no HTTP
client of its own to read headers from (unlike the Go engine). These cover the
parser and the seam the cost flows through, so a client that surfaces headers —
``client.chat.completions.with_raw_response``, or a wrapper that pre-parses — lands
a real ``cost_usd`` on the turn. A real HTTP round-trip is exercised too, against a
local server, to prove the parser works on actual response headers.
"""

from __future__ import annotations

import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from types import SimpleNamespace
from urllib.request import urlopen

import pytest

from smooth_operator_core import (
    AgentOptions,
    CostTracker,
    MockLlmProvider,
    ModelPricing,
    SmoothAgent,
    Usage,
    parse_gateway_cost,
)

PRICING = {"m": ModelPricing(input_per_mtok=1000.0, output_per_mtok=1000.0)}


# ---- parser precedence ----


def test_prefers_margin_then_original_then_legacy() -> None:
    assert (
        parse_gateway_cost(
            {
                "x-litellm-response-cost-margin-amount": "3.0e-05",
                "x-litellm-response-cost-original": "1.0e-05",
                "x-litellm-response-cost": "9.0e-05",
            }
        )
        == 3.0e-05
    )
    assert (
        parse_gateway_cost({"x-litellm-response-cost-original": "1.0e-05", "x-litellm-response-cost": "9.0e-05"})
        == 1.0e-05
    )
    assert parse_gateway_cost({"x-litellm-response-cost": "1.47e-05"}) == 1.47e-05


def test_generic_gateway_fallbacks() -> None:
    assert parse_gateway_cost({"x-response-cost": "0.5"}) == 0.5
    assert parse_gateway_cost({"x-cost-usd": "0.25"}) == 0.25


@pytest.mark.parametrize(
    "headers",
    [
        {},
        None,
        {"x-litellm-response-cost": "0"},
        {"x-litellm-response-cost": "-1"},
        {"x-litellm-response-cost": "not-a-number"},
    ],
)
def test_absent_zero_and_unparseable_are_all_unmeasured(headers) -> None:
    # The distinction the whole fix rests on: absent and zero are BOTH "unmeasured",
    # never a recorded $0.
    assert parse_gateway_cost(headers) is None


def test_zero_margin_falls_through_to_a_real_original() -> None:
    assert (
        parse_gateway_cost(
            {"x-litellm-response-cost-margin-amount": "0", "x-litellm-response-cost-original": "2.5e-05"}
        )
        == 2.5e-05
    )


# ---- against real HTTP response headers ----


class _Handler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802 - stdlib naming
        self.send_response(200)
        self.send_header("x-litellm-response-cost", "1.47e-05")
        self.end_headers()
        self.wfile.write(b"{}")

    def log_message(self, *args) -> None:  # keep the test output quiet
        pass


def test_parses_real_http_response_headers() -> None:
    server = HTTPServer(("127.0.0.1", 0), _Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        with urlopen(f"http://127.0.0.1:{server.server_port}/") as resp:
            # http.client headers are case-insensitive, like httpx's.
            assert parse_gateway_cost(resp.headers) == 1.47e-05
    finally:
        server.shutdown()


# ---- the tracker ----


def test_tracker_prefers_gateway_cost_over_local_estimate() -> None:
    tracker = CostTracker()
    tracker.record_with_gateway_cost("m", Usage(prompt_tokens=10, completion_tokens=5), 0.25, PRICING)
    assert tracker.cost_usd == 0.25
    assert tracker.usage.prompt_tokens == 10


def test_tracker_falls_back_to_local_pricing_when_unmeasured() -> None:
    tracker = CostTracker()
    tracker.record_with_gateway_cost("m", Usage(prompt_tokens=10, completion_tokens=5), None, PRICING)
    assert tracker.cost_usd > 0, "an unmeasured cost must fall back to local pricing, not record 0"


# ---- the turn folds it into cost_usd ----


def _client_returning(**extra):
    """A mock whose response carries whatever cost seam the test exercises."""
    mock = MockLlmProvider()
    # Real token counts, so the local-pricing fallback has something to price —
    # a zero-token response would legitimately cost $0 and prove nothing.
    mock.push_text("hi", usage=SimpleNamespace(prompt_tokens=10, completion_tokens=5))
    original = mock.chat.completions.create

    async def create(**kwargs):
        response = await original(**kwargs)
        for key, value in extra.items():
            object.__setattr__(response, key, value) if hasattr(response, "__slots__") else setattr(
                response, key, value
            )
        return response

    mock.chat = SimpleNamespace(completions=SimpleNamespace(create=create))
    return mock


@pytest.mark.asyncio
async def test_turn_honors_a_precomputed_gateway_cost() -> None:
    agent = SmoothAgent(_client_returning(gateway_cost_usd=0.25), AgentOptions(model="m", pricing=PRICING))
    result = await agent.run("hi")
    assert result.cost_usd == 0.25


@pytest.mark.asyncio
async def test_turn_parses_raw_headers_on_the_response() -> None:
    agent = SmoothAgent(
        _client_returning(headers={"x-litellm-response-cost": "0.75"}),
        AgentOptions(model="m", pricing=PRICING),
    )
    result = await agent.run("hi")
    assert result.cost_usd == 0.75


@pytest.mark.asyncio
async def test_turn_falls_back_to_local_pricing_when_unmeasured() -> None:
    agent = SmoothAgent(_client_returning(), AgentOptions(model="m", pricing=PRICING))
    result = await agent.run("hi")
    assert result.cost_usd > 0
    assert result.cost_usd != 0.25
