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
from typing import Any


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

    def record_with_gateway_cost(
        self,
        model: str,
        usage: Usage,
        gateway_cost_usd: float | None,
        pricing: dict[str, ModelPricing] | None = None,
    ) -> None:
        """Record usage, preferring the gateway's authoritative per-request cost when
        it measured one. ``None`` means "unmeasured" and falls back to the local
        :class:`ModelPricing` estimate — which is the whole reason
        :func:`parse_gateway_cost` returns ``None`` rather than 0. Aliased models
        (``smooth-*``) price at $0 locally, so the gateway's number is often the only
        real one available."""
        if gateway_cost_usd is None:
            self.record(model, usage, pricing)
            return
        self.usage.add(usage)
        self.cost_usd += gateway_cost_usd

    def exceeds(self, budget: CostBudget | None) -> bool:
        if budget is None:
            return False
        if budget.max_tokens is not None and self.usage.total_tokens >= budget.max_tokens:
            return True
        if budget.max_usd is not None and self.cost_usd >= budget.max_usd:
            return True
        return False


#: The response headers the LLM gateway reports per-request cost in, in PRECEDENCE
#: order. Mirrors the Rust reference's candidate list exactly
#: (``rust/smooth-operator-core/src/llm.rs``).
#:
#: LiteLLM splits cost across a few headers: ``-margin-amount`` is what the caller
#: actually pays (includes the gateway's markup), ``-original`` is the raw upstream
#: cost, and the bare ``x-litellm-response-cost`` is the legacy shape older versions
#: emit. The last two are generic fallbacks for other OpenAI-compatible gateways.
GATEWAY_COST_HEADERS: tuple[str, ...] = (
    "x-litellm-response-cost-margin-amount",
    "x-litellm-response-cost-original",
    "x-litellm-response-cost",
    "x-response-cost",
    "x-cost-usd",
)


def parse_gateway_cost(headers: Any) -> float | None:
    """Read the gateway's authoritative per-request cost from response headers,
    taking the FIRST NON-ZERO candidate.

    ``headers`` is anything mapping-like or with a ``.get(name)`` (httpx/requests
    headers are both, and are case-insensitive; a plain dict is tried in a couple of
    cases).

    Returns ``None`` when every candidate is absent OR reports zero. That
    distinction is the whole point: ``None`` means "unmeasured", so the caller falls
    back to local :class:`ModelPricing`, whereas locking in 0 would pin cost at zero
    for the rest of the dispatch. LiteLLM's config on llm.smoo.ai currently reports 0
    for ``smooth-*`` aliases on every response, so taking a zero at face value
    silently zeroes real spend.
    """
    if headers is None:
        return None
    getter = getattr(headers, "get", None)
    if getter is None:
        return None
    for name in GATEWAY_COST_HEADERS:
        raw = getter(name)
        if raw is None:
            # Plain dicts are not case-insensitive the way httpx headers are.
            raw = getter(name.upper())
        if raw is None:
            continue
        try:
            cost = float(str(raw).strip())
        except (TypeError, ValueError):
            continue
        if cost > 0:
            return cost
    return None
