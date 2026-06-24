import { describe, expect, it } from 'vitest';
import { AgentOptions, ChatClientLike, SmoothAgent } from '../src/agent.js';
import { CostBudget, CostTracker, totalTokens, Usage } from '../src/cost.js';

describe('CostTracker', () => {
    it('accumulates usage and cost', () => {
        const t = new CostTracker();
        const pricing = { m: { inputPerMTok: 1.0, outputPerMTok: 2.0 } };
        t.record('m', { promptTokens: 1_000_000, completionTokens: 500_000 }, pricing);
        t.record('m', { promptTokens: 0, completionTokens: 500_000 }, pricing);
        expect(totalTokens(t.usage)).toBe(2_000_000);
        expect(t.costUsd).toBeCloseTo(3.0); // 1*1 + 2*1
    });

    it('counts tokens for an unknown model but costs nothing', () => {
        const t = new CostTracker();
        t.record('unknown', { promptTokens: 100, completionTokens: 50 }, {});
        expect(totalTokens(t.usage)).toBe(150);
        expect(t.costUsd).toBe(0);
    });

    it('budget exceed logic', () => {
        const t = new CostTracker();
        t.usage = { promptTokens: 80, completionTokens: 40 };
        t.costUsd = 0.5;
        expect(t.exceeds(undefined)).toBe(false);
        expect(t.exceeds({ maxTokens: 200 })).toBe(false);
        expect(t.exceeds({ maxTokens: 100 })).toBe(true);
        expect(t.exceeds({ maxUsd: 1.0 })).toBe(false);
        expect(t.exceeds({ maxUsd: 0.5 })).toBe(true);
    });
});

// fake client carrying usage
function fake(scripted: Array<{ content: string | null; tool_calls?: unknown; usage?: { prompt_tokens: number; completion_tokens: number } }>): ChatClientLike {
    let i = 0;
    return {
        chat: {
            completions: {
                create: async () => {
                    const s = scripted[i++];
                    return { choices: [{ message: { content: s.content, tool_calls: s.tool_calls as never } }], usage: s.usage };
                },
            },
        },
    };
}

describe('SmoothAgent cost integration', () => {
    it('reports usage and cost on a run', async () => {
        const client = fake([{ content: 'hi', usage: { prompt_tokens: 1_000_000, completion_tokens: 1_000_000 } }]);
        const agent = new SmoothAgent(client, { model: 'claude-haiku-4-5' } satisfies AgentOptions);
        const res = await agent.run('hello');
        expect(totalTokens(res.usage)).toBe(2_000_000);
        expect(res.costUsd).toBeCloseTo(1.0 + 5.0); // haiku default (1 in, 5 out)
        expect(res.budgetExceeded).toBe(false);
    });

    it('stops when the budget is exceeded', async () => {
        const client = fake([
            { content: null, tool_calls: [{ id: 'c1', function: { name: 'noop', arguments: '{}' } }], usage: { prompt_tokens: 200, completion_tokens: 0 } },
        ]);
        const budget: CostBudget = { maxTokens: 100 };
        const agent = new SmoothAgent(client, { model: 'claude-haiku-4-5', budget });
        const res = await agent.run('go');
        expect(res.budgetExceeded).toBe(true);
        expect(res.iterations).toBe(1);
        expect(res.toolCalls).toBe(0);
    });
});
