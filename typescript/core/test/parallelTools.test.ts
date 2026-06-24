/**
 * Unit tests for concurrent (parallel) tool-call execution.
 *
 * When `AgentOptions.parallelToolCalls` is true and an assistant turn returns ≥2
 * tool calls, the dispatches run concurrently (`Promise.all`) — but the tool-result
 * messages must still be appended in the original `tool_calls` order so the
 * transcript is deterministic. Default (false) keeps sequential dispatch.
 */

import { describe, expect, it } from 'vitest';
import { Tool } from '../src/agent.js';
import { SmoothAgent } from '../src/agent.js';
import { MockLlmProvider, ScriptedMessage } from '../src/llmProvider.js';

/** An OpenAI-shaped assistant message requesting several tool calls at once. */
function multiToolCall(...calls: Array<[string, string, string]>): ScriptedMessage {
    return {
        content: null,
        tool_calls: calls.map(([id, name, args]) => ({ id, function: { name, arguments: args } })),
    };
}

/** A deferred promise + resolve handle. */
function gate(): { promise: Promise<void>; release: () => void } {
    let release!: () => void;
    const promise = new Promise<void>((resolve) => {
        release = resolve;
    });
    return { promise, release };
}

describe('parallel tool calls', () => {
    it('overlaps tool dispatches when enabled', async () => {
        // Two tools that each block until both have started — only completes if concurrent.
        let started = 0;
        const bothStarted = gate();
        const slow: (name: string) => Tool = (name) => ({
            name,
            description: '',
            parameters: { type: 'object' },
            execute: async () => {
                started++;
                if (started === 2) bothStarted.release();
                await bothStarted.promise;
                return name;
            },
        });
        const client = new MockLlmProvider();
        client.pushResponse(multiToolCall(['c1', 'a', '{}'], ['c2', 'b', '{}'])).pushText('done');
        const agent = new SmoothAgent(client, { tools: [slow('a'), slow('b')], parallelToolCalls: true });
        const result = await agent.run('go');
        expect(result.text).toBe('done');
        expect(result.toolCalls).toBe(2);
    });

    it('preserves tool-result order despite scrambled completion', async () => {
        const gates: Record<string, ReturnType<typeof gate>> = { A: gate(), B: gate(), C: gate() };
        const make: (name: string) => Tool = (name) => ({
            name,
            description: '',
            parameters: { type: 'object' },
            execute: async () => {
                await gates[name].promise;
                return `result-${name}`;
            },
        });
        const client = new MockLlmProvider();
        client.pushResponse(multiToolCall(['c1', 'A', '{}'], ['c2', 'B', '{}'], ['c3', 'C', '{}'])).pushText('done');
        const agent = new SmoothAgent(client, { tools: [make('A'), make('B'), make('C')], parallelToolCalls: true });

        const run = agent.run('go');
        // Finish in B, C, A order — opposite of transcript order for A.
        await new Promise((r) => setTimeout(r, 5));
        gates.B.release();
        await new Promise((r) => setTimeout(r, 5));
        gates.C.release();
        await new Promise((r) => setTimeout(r, 5));
        gates.A.release();
        await run;

        const toolResults = client.calls[1].messages.filter((m) => m.role === 'tool').map((m) => m.content);
        expect(toolResults).toEqual(['result-A', 'result-B', 'result-C']);
    });

    it('keeps a failing tool in its correct position', async () => {
        const ok: (name: string) => Tool = (name) => ({
            name,
            description: '',
            parameters: { type: 'object' },
            execute: async () => 'ok',
        });
        const boom: Tool = {
            name: 'B',
            description: '',
            parameters: { type: 'object' },
            execute: async () => {
                throw new Error('kaboom');
            },
        };
        const client = new MockLlmProvider();
        client.pushResponse(multiToolCall(['c1', 'A', '{}'], ['c2', 'B', '{}'], ['c3', 'C', '{}'])).pushText('done');
        const agent = new SmoothAgent(client, { tools: [ok('A'), boom, ok('C')], parallelToolCalls: true });
        await agent.run('go');

        const toolResults = client.calls[1].messages.filter((m) => m.role === 'tool').map((m) => String(m.content));
        expect(toolResults[0]).toBe('ok');
        expect(toolResults[1]).toContain('kaboom');
        expect(toolResults[2]).toBe('ok');
    });

    it('dispatches sequentially when the flag is off (default)', async () => {
        const order: string[] = [];
        const make: (name: string) => Tool = (name) => ({
            name,
            description: '',
            parameters: { type: 'object' },
            execute: async () => {
                order.push(name);
                return name;
            },
        });
        const client = new MockLlmProvider();
        client.pushResponse(multiToolCall(['c1', 'A', '{}'], ['c2', 'B', '{}'])).pushText('done');
        const agent = new SmoothAgent(client, { tools: [make('A'), make('B')] }); // parallelToolCalls defaults false
        const result = await agent.run('go');
        expect(order).toEqual(['A', 'B']);
        expect(result.toolCalls).toBe(2);
    });

    it('handles a single tool call identically with the flag on', async () => {
        const echo: Tool = {
            name: 'echo',
            description: '',
            parameters: { type: 'object' },
            execute: async (args) => String(args.text ?? ''),
        };
        const client = new MockLlmProvider();
        client.pushToolCall('c1', 'echo', '{"text":"hi"}').pushText('done');
        const agent = new SmoothAgent(client, { tools: [echo], parallelToolCalls: true });
        const result = await agent.run('go');
        expect(result.text).toBe('done');
        expect(result.toolCalls).toBe(1);
    });
});
