import { describe, expect, it } from 'vitest';
import { ChatClientLike, SmoothAgent } from '../src/agent.js';
import { InMemoryKnowledge, KnowledgeHit } from '../src/knowledge.js';
import { LexicalReranker, NoopReranker } from '../src/rerank.js';

const hit = (content: string, source = 's'): KnowledgeHit => ({ content, source, score: 0 });

describe('reranking', () => {
    it('NoopReranker is passthrough', () => {
        const hits = [hit('a'), hit('b')];
        expect(new NoopReranker().rerank('q', hits)).toEqual(hits);
    });

    it('LexicalReranker prefers a concise doc over a long one with same coverage', () => {
        const concise = hit('return policy');
        const verbose = hit(`return ${'filler '.repeat(60)}policy`);
        const out = new LexicalReranker().rerank('return policy', [verbose, concise]);
        expect(out[0]).toBe(concise);
        expect(out[1]).toBe(verbose);
    });

    it('LexicalReranker prefers higher coverage', () => {
        const coversTwo = hit('return and policy details');
        const coversOne = hit('return shipping details');
        const out = new LexicalReranker().rerank('return policy', [coversOne, coversTwo]);
        expect(out[0]).toBe(coversTwo);
    });

    it('empty query is passthrough', () => {
        const hits = [hit('a'), hit('b')];
        expect(new LexicalReranker().rerank('', hits)).toEqual(hits);
    });

    it('is applied between retrieval and injection', async () => {
        const kb = new InMemoryKnowledge();
        kb.ingest(`return ${'filler '.repeat(60)}policy`, 'long.md');
        kb.ingest('return policy', 'short.md');

        let systemContent = '';
        const client: ChatClientLike = {
            chat: {
                completions: {
                    create: async (body: Record<string, unknown>) => {
                        const messages = body.messages as Array<Record<string, unknown>>;
                        systemContent = (messages[0].content as string) ?? '';
                        return { choices: [{ message: { content: 'ok' } }] };
                    },
                },
            },
        };
        const agent = new SmoothAgent(client, {
            instructions: 'support',
            knowledge: kb,
            knowledgeTopK: 1,
            knowledgeCandidateK: 2,
            reranker: new LexicalReranker(),
        });
        await agent.run('return policy');
        expect(systemContent).toContain('[short.md]');
        expect(systemContent).not.toContain('[long.md]');
    });
});
