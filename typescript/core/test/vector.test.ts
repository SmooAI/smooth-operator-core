import { describe, expect, it } from 'vitest';
import { ChatClientLike, SmoothAgent } from '../src/agent.js';
import { HashEmbedder, VectorKnowledge } from '../src/vector.js';

describe('vector knowledge', () => {
    it('HashEmbedder is deterministic and L2-normalized', () => {
        const emb = new HashEmbedder(64);
        const a = emb.embed('return policy details');
        const b = emb.embed('return policy details');
        expect(a).toEqual(b);
        const norm = Math.sqrt(a.reduce((s, v) => s + v * v, 0));
        expect(norm).toBeCloseTo(1.0);
        expect(a).toHaveLength(64);
    });

    it('retrieves the most similar document', () => {
        const kb = new VectorKnowledge(new HashEmbedder(256));
        kb.ingest('Our return policy allows refunds within 30 days.', 'returns.md');
        kb.ingest('The office is open Monday through Friday.', 'hours.md');
        const hits = kb.query('how do refunds and returns work?', 1);
        expect(hits).toHaveLength(1);
        expect(hits[0].source).toBe('returns.md');
        expect(hits[0].score).toBeGreaterThan(0);
    });

    it('empty store returns nothing', () => {
        expect(new VectorKnowledge().query('anything', 4)).toEqual([]);
    });

    it('the agent accepts vector knowledge (satisfies the Knowledge interface)', async () => {
        const kb = new VectorKnowledge();
        kb.ingest('Gift wrapping costs 4.99 per item.', 'wrapping.md');
        kb.ingest('Returns are accepted within 30 days.', 'returns.md');

        let systemContent = '';
        const client: ChatClientLike = {
            chat: {
                completions: {
                    create: async (body: Record<string, unknown>) => {
                        const messages = body.messages as Array<Record<string, unknown>>;
                        systemContent = (messages[0].content as string) ?? '';
                        return { choices: [{ message: { content: "It's 4.99 per item." } }] };
                    },
                },
            },
        };
        const agent = new SmoothAgent(client, { instructions: 'support', knowledge: kb, knowledgeTopK: 1 });
        await agent.run('how much is gift wrapping?');
        expect(systemContent).toContain('[wrapping.md]');
    });
});
