/**
 * Non-network unit tests for the TypeScript core: the agentic loop, tool calling,
 * and knowledge injection, driven by the reusable `MockLlmProvider`. Always green
 * (no credentials) — live-gateway behavior is covered by `evals.test.ts`.
 *
 * These tests used to roll their own ad-hoc `FakeClient`; they now use the shared
 * `MockLlmProvider` (see `llmProvider.test.ts`) as the demonstration that it
 * replaces the hand-written fakes.
 */

import { describe, expect, it } from 'vitest';
import { AgentOptions, SmoothAgent, Tool } from '../src/agent.js';
import { InMemoryKnowledge } from '../src/knowledge.js';
import { MockLlmProvider } from '../src/llmProvider.js';

function makeAgent(client: MockLlmProvider, options: AgentOptions = {}): SmoothAgent {
    return new SmoothAgent(client, options);
}

describe('InMemoryKnowledge', () => {
    it('ranks by token overlap', () => {
        const kb = new InMemoryKnowledge();
        kb.ingest('The return window is 17 days from delivery.', 'returns.md');
        kb.ingest('Gift wrapping costs 4.99 per item.', 'wrapping.md');
        const hits = kb.query('what is the return window?', 1);
        expect(hits).toHaveLength(1);
        expect(hits[0].content).toContain('17 days');
    });
});

describe('SmoothAgent', () => {
    it('stops after one call on a text reply', async () => {
        const client = new MockLlmProvider().pushText('the answer is 42');
        const agent = makeAgent(client, { instructions: 'be helpful' });
        const result = await agent.run('what is the answer?');
        expect(result.text).toBe('the answer is 42');
        expect(result.iterations).toBe(1);
        expect(result.toolCalls).toBe(0);
    });

    it('runs a tool then finishes', async () => {
        const echo: Tool = {
            name: 'echo',
            description: 'Echoes input back',
            parameters: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] },
            execute: async (args) => String(args.text ?? ''),
        };
        const client = new MockLlmProvider();
        client.pushToolCall('call-1', 'echo', '{"text": "hello tools"}').pushText('done');
        const agent = makeAgent(client, { tools: [echo] });
        const result = await agent.run('use echo');
        expect(result.text).toBe('done');
        expect(result.toolCalls).toBe(1);
        // The tool result was fed back as a tool-role message before the final call.
        const secondCallMessages = client.calls[1].messages;
        expect(secondCallMessages.some((m) => m.role === 'tool' && m.content === 'hello tools')).toBe(true);
    });

    it('injects knowledge into the system prompt', async () => {
        const kb = new InMemoryKnowledge();
        kb.ingest('The return window is exactly 17 days from delivery.', 'returns.md');
        const client = new MockLlmProvider().pushText('17 days');
        const agent = makeAgent(client, { instructions: 'support agent', knowledge: kb });
        await agent.run('how many days to return?');
        const messages = client.calls[0].messages;
        expect(messages[0].role).toBe('system');
        expect(messages[0].content).toContain('17 days');
    });
});
