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

    exceeds(budget?: CostBudget): boolean {
        if (!budget) return false;
        if (budget.maxTokens !== undefined && totalTokens(this.usage) >= budget.maxTokens) return true;
        if (budget.maxUsd !== undefined && this.costUsd >= budget.maxUsd) return true;
        return false;
    }
}
