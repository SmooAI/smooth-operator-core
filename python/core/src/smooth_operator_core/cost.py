"""Token-usage accounting and budget enforcement.

Phase-1 sibling of the reference engines' cost tracking. Accumulates token usage
across a turn's model calls, optionally converts it to a USD cost via a per-model
pricing table, and lets a turn stop early once a token or cost budget is hit.

Usage is exact (reported by the model API). Cost depends on the pricing table,
which is approximate and meant to be overridden — pass your own
``ModelPricing`` map via :class:`AgentOptions`.
"""

from __future__ import annotations

from dataclasses import dataclass, field


@dataclass
class Usage:
    """Exact token counts reported by the model API."""

    prompt_tokens: int = 0
    completion_tokens: int = 0

    @property
    def total_tokens(self) -> int:
        return self.prompt_tokens + self.completion_tokens

    def add(self, other: "Usage") -> None:
        self.prompt_tokens += other.prompt_tokens
        self.completion_tokens += other.completion_tokens


@dataclass(frozen=True)
class ModelPricing:
    """USD per 1,000,000 tokens, input and output."""

    input_per_mtok: float
    output_per_mtok: float

    def cost(self, usage: Usage) -> float:
        return (usage.prompt_tokens * self.input_per_mtok + usage.completion_tokens * self.output_per_mtok) / 1_000_000


#: Approximate default pricing (USD / 1M tokens). Override via AgentOptions.pricing.
DEFAULT_PRICING: dict[str, ModelPricing] = {
    "claude-haiku-4-5": ModelPricing(1.0, 5.0),
    "claude-sonnet-4-5": ModelPricing(3.0, 15.0),
}


@dataclass
class CostBudget:
    """A ceiling for a turn. Either limit may be set; the first hit stops the turn."""

    max_usd: float | None = None
    max_tokens: int | None = None


@dataclass
class CostTracker:
    """Accumulates usage + cost across a turn's model calls."""

    usage: Usage = field(default_factory=Usage)
    cost_usd: float = 0.0

    def record(self, model: str, usage: Usage, pricing: dict[str, ModelPricing] | None = None) -> None:
        self.usage.add(usage)
        table = pricing if pricing is not None else DEFAULT_PRICING
        mp = table.get(model)
        if mp is not None:
            self.cost_usd += mp.cost(usage)

    def exceeds(self, budget: CostBudget | None) -> bool:
        if budget is None:
            return False
        if budget.max_tokens is not None and self.usage.total_tokens >= budget.max_tokens:
            return True
        if budget.max_usd is not None and self.cost_usd >= budget.max_usd:
            return True
        return False
