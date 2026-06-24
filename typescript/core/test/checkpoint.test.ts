import { describe, expect, it } from 'vitest';
import { ChatClientLike, SmoothAgent } from '../src/agent.js';
import { InMemoryCheckpointStore } from '../src/checkpoint.js';

function fake(scripted: Array<{ content: string | null }>): { client: ChatClientLike; calls: Array<Array<Record<string, unknown>>> } {
    const calls: Array<Array<Record<string, unknown>>> = [];
    let i = 0;
    const client: ChatClientLike = {
        chat: {
            completions: {
                create: async (body: Record<string, unknown>) => {
                    calls.push(body.messages as Array<Record<string, unknown>>);
                    return { choices: [{ message: { content: scripted[i++].content } }] };
                },
            },
        },
    };
    return { client, calls };
}

describe('checkpointing', () => {
    it('saves and loads a checkpoint round-trip', () => {
        const store = new InMemoryCheckpointStore();
        expect(store.load('missing')).toBeUndefined();
        store.save({ conversationId: 'c1', messages: [{ role: 'user', content: 'hi' }] });
        expect(store.load('c1')?.messages).toEqual([{ role: 'user', content: 'hi' }]);
    });

    it('persists and resumes the conversation across turns', async () => {
        const store = new InMemoryCheckpointStore();
        const { client, calls } = fake([{ content: 'first answer' }, { content: 'second answer' }]);
        const agent = new SmoothAgent(client, { checkpointStore: store, conversationId: 'conv-1' });

        await agent.run('hello');
        const cp = store.load('conv-1');
        expect(cp?.messages.map((m) => m.role)).toEqual(['user', 'assistant']);
        expect(cp?.messages[0].content).toBe('hello');
        expect(cp?.messages[1].content).toBe('first answer');

        await agent.run('again');
        const secondCall = calls[1];
        const contents = secondCall.map((m) => m.content);
        expect(contents).toContain('hello');
        expect(contents).toContain('first answer');
        expect(contents).toContain('again');

        expect(store.load('conv-1')?.messages.map((m) => m.role)).toEqual(['user', 'assistant', 'user', 'assistant']);
    });

    it('does not checkpoint when conversationId is unset', async () => {
        const store = new InMemoryCheckpointStore();
        const { client } = fake([{ content: 'hi' }]);
        const agent = new SmoothAgent(client, { checkpointStore: store });
        await agent.run('hello');
        expect(store.load('conv-1')).toBeUndefined();
    });
});
