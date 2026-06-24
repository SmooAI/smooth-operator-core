"""Retry-with-exponential-backoff around the model call.

Driven by the reusable :class:`MockLlmProvider` (it can script transient errors via
``push_error``). Backoff is set to 0 so no real time is spent sleeping.
"""

from __future__ import annotations

import pytest

from smooth_operator_core import AgentOptions, MockLlmProvider, SmoothAgent


@pytest.mark.asyncio
async def test_retries_then_succeeds():
    # Errors k times, then a text reply; max_retries >= k → the turn succeeds and the
    # model is called exactly k+1 times.
    mock = MockLlmProvider()
    mock.push_error("rate limited").push_error("rate limited").push_text("ok")
    agent = SmoothAgent(mock, AgentOptions(max_retries=2, retry_backoff_ms=0))
    result = await agent.run("hi")
    assert result.text == "ok"
    assert mock.call_count == 3  # k+1 = 2 failures + 1 success


@pytest.mark.asyncio
async def test_error_propagates_when_retries_exhausted():
    # Errors max_retries+1 times → the provider error propagates (the turn fails).
    mock = MockLlmProvider()
    mock.push_error("boom").push_error("boom")
    agent = SmoothAgent(mock, AgentOptions(max_retries=1, retry_backoff_ms=0))
    with pytest.raises(Exception, match="boom"):
        await agent.run("hi")
    assert mock.call_count == 2  # max_retries + 1 attempts


@pytest.mark.asyncio
async def test_no_retry_by_default():
    # Default max_retries=0 → a single error propagates immediately (one attempt).
    mock = MockLlmProvider()
    mock.push_error("nope").push_text("never reached")
    agent = SmoothAgent(mock, AgentOptions())
    with pytest.raises(Exception, match="nope"):
        await agent.run("hi")
    assert mock.call_count == 1
