import { describe, expect, it } from 'vitest';
import { ChatClientLike, SmoothAgent, Tool } from '../src/agent.js';
import { SmoothAgentThread } from '../src/thread.js';

type ScriptedResponse = {
    content: string | null;
    tool_calls?: Array<{ id: string; function: { name: string; arguments: string } }>;
};

// Records a *copy* of each call's messages so assertions reflect what the model
// saw at call time (the live array keeps mutating as the turn appends messages).
function fake(scripted: ScriptedResponse[]): { client: ChatClientLike; calls: Array<Array<Record<string, unknown>>> } {
    const calls: Array<Array<Record<string, unknown>>> = [];
    let i = 0;
    const client: ChatClientLike = {
        chat: {
            completions: {
                create: async (body: Record<string, unknown>) => {
                    calls.push((body.messages as Array<Record<string, unknown>>).map((m) => ({ ...m })));
                    const r = scripted[i++];
                    return { choices: [{ message: { content: r.content, tool_calls: r.tool_calls ?? null } }] };
                },
            },
        },
    };
    return { client, calls };
}

describe('SmoothAgentThread', () => {
    it('a fresh thread has an id and no messages', () => {
        const t = new SmoothAgentThread();
        expect(t.id).toBeTruthy();
        expect(t.length).toBe(0);
        expect(t.messages).toEqual([]);
        // Two fresh threads get distinct ids.
        expect(new SmoothAgentThread().id).not.toBe(new SmoothAgentThread().id);
    });

    it('resumes with an explicit id', () => {
        expect(new SmoothAgentThread('conv-42').id).toBe('conv-42');
    });

    it('never stores system messages', () => {
        const t = new SmoothAgentThread();
        t.add({ role: 'system', content: 'you are helpful' });
        t.add({ role: 'user', content: 'hi' });
        t.extend([
            { role: 'system', content: 'ignored' },
            { role: 'assistant', content: 'hello' },
        ]);
        expect(t.messages.map((m) => m.role)).toEqual(['user', 'assistant']);
    });

    it('carries history across two sequential runs', async () => {
        const { client, calls } = fake([{ content: 'first answer' }, { content: 'second answer' }]);
        const agent = new SmoothAgent(client, { instructions: 'be helpful' });
        const thread = new SmoothAgentThread();

        // Turn 1 — seeds nothing prior; appends [user, assistant] to the thread.
        await agent.run('hello', undefined, thread);
        expect(thread.messages.map((m) => m.role)).toEqual(['user', 'assistant']);
        expect(thread.messages[0].content).toBe('hello');
        expect(thread.messages[1].content).toBe('first answer');

        // Turn 2 — the second model call must see turn 1's history.
        await agent.run('again', undefined, thread);
        const secondCall = calls[1];
        const contents = secondCall.map((m) => m.content);
        expect(contents).toContain('hello');
        expect(contents).toContain('first answer');
        expect(contents).toContain('again');
        // No system message is ever stored on the thread.
        expect(secondCall.some((m) => m.role === 'system' && m.content === 'be helpful')).toBe(true);

        // The thread now holds the full 4-message conversation, no system message.
        expect(thread.messages.map((m) => m.role)).toEqual(['user', 'assistant', 'user', 'assistant']);
        expect(thread.messages.every((m) => m.role !== 'system')).toBe(true);
    });

    it('seeds no prior history on the first run', async () => {
        const { client, calls } = fake([{ content: 'hi there' }]);
        const agent = new SmoothAgent(client, { instructions: 'be helpful' });
        const thread = new SmoothAgentThread();

        await agent.run('hello', undefined, thread);
        // The only model call: system + the single user message, nothing prior.
        expect(calls[0].map((m) => m.role)).toEqual(['system', 'user']);
    });

    it('accumulates tool messages on the thread', async () => {
        const { client } = fake([
            { content: '', tool_calls: [{ id: 'call-1', function: { name: 'echo', arguments: '{"text":"hi"}' } }] },
            { content: 'done' },
        ]);
        const echo: Tool = {
            name: 'echo',
            description: 'echo',
            parameters: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] },
            async execute(args) {
                return String(args.text ?? '');
            },
        };
        const agent = new SmoothAgent(client, { tools: [echo] });
        const thread = new SmoothAgentThread();

        await agent.run('please echo', undefined, thread);
        // user, assistant(tool_call), tool result, assistant(final answer)
        expect(thread.messages.map((m) => m.role)).toEqual(['user', 'assistant', 'tool', 'assistant']);
        expect(thread.messages.every((m) => m.role !== 'system')).toBe(true);
    });

    it('single-shot run still works without a thread', async () => {
        const { client } = fake([{ content: 'the answer is 42' }]);
        const agent = new SmoothAgent(client, { instructions: 'be helpful' });
        const res = await agent.run('what is the answer?');
        expect(res.text).toBe('the answer is 42');
        expect(res.iterations).toBe(1);
        expect(res.toolCalls).toBe(0);
    });
});
