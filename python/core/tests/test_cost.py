"""Unit tests for token-usage accounting and budget enforcement."""

from __future__ import annotations

from types import SimpleNamespace

import pytest

from smooth_operator_core import AgentOptions, CostBudget, CostTracker, ModelPricing, SmoothAgent, Usage


def test_cost_tracker_accumulates_usage_and_cost():
    tracker = CostTracker()
    pricing = {"m": ModelPricing(input_per_mtok=1.0, output_per_mtok=2.0)}
    tracker.record("m", Usage(prompt_tokens=1_000_000, completion_tokens=500_000), pricing)
    tracker.record("m", Usage(prompt_tokens=0, completion_tokens=500_000), pricing)
    assert tracker.usage.prompt_tokens == 1_000_000
    assert tracker.usage.completion_tokens == 1_000_000
    assert tracker.usage.total_tokens == 2_000_000
    # 1.0*1M input + 2.0*1M output = 1.0 + 2.0 = 3.0
    assert tracker.cost_usd == pytest.approx(3.0)


def test_unknown_model_costs_nothing_but_still_counts_tokens():
    tracker = CostTracker()
    tracker.record("unknown-model", Usage(prompt_tokens=100, completion_tokens=50), pricing={})
    assert tracker.usage.total_tokens == 150
    assert tracker.cost_usd == 0.0


def test_budget_exceeds_logic():
    tracker = CostTracker(usage=Usage(prompt_tokens=80, completion_tokens=40), cost_usd=0.5)
    assert tracker.exceeds(None) is False
    assert tracker.exceeds(CostBudget(max_tokens=200)) is False
    assert tracker.exceeds(CostBudget(max_tokens=100)) is True  # 120 >= 100
    assert tracker.exceeds(CostBudget(max_usd=1.0)) is False
    assert tracker.exceeds(CostBudget(max_usd=0.5)) is True


# ── fake client carrying usage, for the agent budget-stop test ───────────────
def _resp(content=None, tool_calls=None, usage=(0, 0)):
    msg = SimpleNamespace(content=content, tool_calls=tool_calls)
    u = SimpleNamespace(prompt_tokens=usage[0], completion_tokens=usage[1])
    return SimpleNamespace(choices=[SimpleNamespace(message=msg)], usage=u)


class _FakeCompletions:
    def __init__(self, scripted):
        self._scripted = list(scripted)

    async def create(self, **_kwargs):
        return self._scripted.pop(0)


class FakeClient:
    def __init__(self, scripted):
        self.chat = SimpleNamespace(completions=_FakeCompletions(scripted))


@pytest.mark.asyncio
async def test_run_reports_usage_and_cost():
    client = FakeClient([_resp(content="hi", usage=(1_000_000, 1_000_000))])
    agent = SmoothAgent(client, AgentOptions(model="claude-haiku-4-5"))
    result = await agent.run("hello")
    assert result.usage.total_tokens == 2_000_000
    # claude-haiku-4-5 default pricing = (1.0 in, 5.0 out) per 1M.
    assert result.cost_usd == pytest.approx(1.0 + 5.0)
    assert result.budget_exceeded is False


@pytest.mark.asyncio
async def test_run_stops_when_budget_exceeded():
    # The model wants a tool call (would loop), but the budget is hit first.
    tc = SimpleNamespace(id="c1", function=SimpleNamespace(name="noop", arguments="{}"))
    client = FakeClient([_resp(content=None, tool_calls=[tc], usage=(200, 0))])
    agent = SmoothAgent(client, AgentOptions(model="claude-haiku-4-5", budget=CostBudget(max_tokens=100)))
    result = await agent.run("go")
    assert result.budget_exceeded is True
    assert result.iterations == 1
    assert result.tool_calls == 0  # stopped before executing the tool
