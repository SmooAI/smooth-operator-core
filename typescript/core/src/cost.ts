/**
 * Token-usage accounting and budget enforcement.
 *
 * Phase-1 sibling of the reference engines' cost tracking. Accumulates token
 * usage across a turn's model calls, optionally converts it to a USD cost via a
 * per-model pricing table, and lets a turn stop early once a token or cost budget
 * is hit. Usage is exact; cost depends on the (approximate, overridable) pricing.
 */

export interface Usage {
    promptTokens: number;
    completionTokens: number;
}

export function totalTokens(u: Usage): number {
    return u.promptTokens + u.completionTokens;
}

/** USD per 1,000,000 tokens, input and output. */
export interface ModelPricing {
    inputPerMTok: number;
    outputPerMTok: number;
}

export function pricingCost(p: ModelPricing, u: Usage): number {
    return (u.promptTokens * p.inputPerMTok + u.completionTokens * p.outputPerMTok) / 1_000_000;
}

/** Approximate default pricing (USD / 1M tokens). Override via AgentOptions.pricing. */
export const DEFAULT_PRICING: Record<string, ModelPricing> = {
    'claude-haiku-4-5': { inputPerMTok: 1.0, outputPerMTok: 5.0 },
    'claude-sonnet-4-5': { inputPerMTok: 3.0, outputPerMTok: 15.0 },
};

/**
 * The response headers the LLM gateway reports per-request cost in, in PRECEDENCE
 * order. Mirrors the Rust reference's candidate list exactly
 * (`rust/smooth-operator-core/src/llm.rs`).
 *
 * LiteLLM splits cost across a few headers: `-margin-amount` is what the caller
 * actually pays (includes the gateway's markup), `-original` is the raw upstream
 * cost, and the bare `x-litellm-response-cost` is the legacy shape older versions
 * emit. The last two are generic fallbacks for other OpenAI-compatible gateways.
 */
export const GATEWAY_COST_HEADERS: readonly string[] = [
    'x-litellm-response-cost-margin-amount',
    'x-litellm-response-cost-original',
    'x-litellm-response-cost',
    'x-response-cost',
    'x-cost-usd',
];

/** Anything that can look up a header by (case-insensitive) name. */
export type HeaderLike = Headers | Record<string, string | undefined> | { get(name: string): string | null | undefined };

function headerValue(headers: HeaderLike, name: string): string | undefined {
    if (typeof (headers as { get?: unknown }).get === 'function') {
        return (headers as { get(n: string): string | null | undefined }).get(name) ?? undefined;
    }
    const record = headers as Record<string, string | undefined>;
    // Plain objects are not case-insensitive the way Headers is.
    return record[name] ?? record[name.toLowerCase()] ?? record[name.toUpperCase()];
}

/**
 * Read the gateway's authoritative per-request cost from response headers, taking
 * the FIRST NON-ZERO candidate.
 *
 * Returns `undefined` when every candidate is absent OR reports zero. That
 * distinction is the whole point: `undefined` means "unmeasured", so the caller
 * falls back to local {@link ModelPricing}, whereas locking in 0 would pin cost at
 * zero for the rest of the dispatch. LiteLLM's config on llm.smoo.ai currently
 * reports 0 for `smooth-*` aliases on every response, so taking a zero at face
 * value silently zeroes real spend.
 */
export function parseGatewayCost(headers: HeaderLike | null | undefined): number | undefined {
    if (!headers) return undefined;
    for (const name of GATEWAY_COST_HEADERS) {
        const raw = headerValue(headers, name);
        if (raw === undefined || raw === null) continue;
        const cost = Number.parseFloat(String(raw).trim());
        if (Number.isFinite(cost) && cost > 0) return cost;
    }
    return undefined;
}

/** A ceiling for a turn. Either limit may be set; the first hit stops the turn. */
export interface CostBudget {
    maxUsd?: number;
    maxTokens?: number;
}

/** Accumulates usage + cost across a turn's model calls. */
export class CostTracker {
    usage: Usage = { promptTokens: 0, completionTokens: 0 };
    costUsd = 0;

    record(model: string, usage: Usage, pricing?: Record<string, ModelPricing>): void {
        this.usage.promptTokens += usage.promptTokens;
        this.usage.completionTokens += usage.completionTokens;
        const table = pricing ?? DEFAULT_PRICING;
        const mp = table[model];
        if (mp) this.costUsd += pricingCost(mp, usage);
    }

    /**
     * Record usage, preferring the gateway's authoritative per-request cost when it
     * measured one. `undefined` means "unmeasured" and falls back to the local
     * {@link ModelPricing} estimate — which is the whole reason
     * {@link parseGatewayCost} returns `undefined` rather than 0. Aliased models
     * (`smooth-*`) price at $0 locally, so the gateway's number is often the only
     * real one available.
     */
    recordWithGatewayCost(
        model: string,
        usage: Usage,
        gatewayCostUsd: number | undefined,
        pricing?: Record<string, ModelPricing>,
    ): void {
        if (gatewayCostUsd === undefined) {
            this.record(model, usage, pricing);
            return;
        }
        this.usage.promptTokens += usage.promptTokens;
        this.usage.completionTokens += usage.completionTokens;
        this.costUsd += gatewayCostUsd;
    }

    exceeds(budget?: CostBudget): boolean {
        if (!budget) return false;
        if (budget.maxTokens !== undefined && totalTokens(this.usage) >= budget.maxTokens) return true;
        if (budget.maxUsd !== undefined && this.costUsd >= budget.maxUsd) return true;
        return false;
    }
}
