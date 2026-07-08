"""The model-output-token ceiling clamp (EPIC th-1cc9fa / th-562b6d).

A budget/policy ``max_tokens`` can exceed what a model can physically emit — a
reasoning model then burns the whole budget on reasoning and returns EMPTY, or the
upstream 400s. :func:`effective_max_tokens` clamps every request's ``max_tokens``
to ``min(configured, model_max_output)``; ``None`` (or ``<= 0``) leaves it
unclamped (graceful passthrough).

Mirrors the Rust reference's ``effective_max_tokens_clamps_to_model_ceiling`` test
in ``rust/smooth-operator-core/src/llm.rs``.
"""

from __future__ import annotations

import pytest

from smooth_operator_core import AgentOptions, MockLlmProvider, SmoothAgent, effective_max_tokens

# ── the pure clamp (the same table the Rust reference asserts) ───────────────


def test_no_ceiling_passthrough():
    # Unknown ceiling ⇒ the configured budget is sent unchanged.
    assert effective_max_tokens(32_768, None) == 32_768


def test_ceiling_below_budget_clamps_down():
    # A ceiling under the budget wins — this is the whole point.
    assert effective_max_tokens(32_768, 8_192) == 8_192


def test_ceiling_above_budget_keeps_budget():
    # A roomy ceiling never RAISES the budget — min() keeps the smaller.
    assert effective_max_tokens(32_768, 384_000) == 32_768


def test_ceiling_equal_to_budget():
    assert effective_max_tokens(32_768, 32_768) == 32_768


def test_zero_ceiling_treated_as_unknown():
    # A 0/negative ceiling is nonsense ⇒ treat as unknown, never clamp to 0
    # (a 0 budget would make the model emit nothing).
    assert effective_max_tokens(32_768, 0) == 32_768
    assert effective_max_tokens(32_768, -5) == 32_768


def test_never_returns_zero_even_when_budget_zero():
    # Defensive: a misconfigured 0 budget with a real ceiling still floors at 1.
    assert effective_max_tokens(0, 8_192) == 1


# ── the agent actually SENDS the clamped value on both call paths ────────────


@pytest.mark.asyncio
async def test_run_sends_clamped_max_tokens():
    mock = MockLlmProvider()
    mock.push_text("hi")
    agent = SmoothAgent(mock, AgentOptions(max_tokens=32_768, model_max_output=8_192))
    await agent.run("hello")
    assert mock.last_call is not None
    assert mock.last_call.kwargs["max_tokens"] == 8_192


@pytest.mark.asyncio
async def test_run_passthrough_when_no_ceiling():
    mock = MockLlmProvider()
    mock.push_text("hi")
    agent = SmoothAgent(mock, AgentOptions(max_tokens=32_768))  # model_max_output defaults to None
    await agent.run("hello")
    assert mock.last_call is not None
    assert mock.last_call.kwargs["max_tokens"] == 32_768


@pytest.mark.asyncio
async def test_run_stream_sends_clamped_max_tokens():
    mock = MockLlmProvider()
    mock.push_text("streamed")
    agent = SmoothAgent(mock, AgentOptions(max_tokens=32_768, model_max_output=8_192))
    async for _ in agent.run_stream("hello"):
        pass
    assert mock.last_call is not None
    assert mock.last_call.kwargs["max_tokens"] == 8_192
