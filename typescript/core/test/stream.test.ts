/**
 * Unit tests for streaming turn execution (`SmoothAgent.runStream`).
 *
 * Proves the streaming loop mirrors the C# `RunStreamingAsync` behaviour: text
 * deltas surface as multiple `text` events, tool calls round-trip through the same
 * dispatch path (clearance/human-gate/JSON parsing), arguments split across chunks
 * still assemble correctly, and the terminal `done` event carries a response
 * equivalent to non-streaming `run()`.
 */

import { describe, expect, it } from 'vitest';
import { AgentOptions, SmoothAgent, StreamEvent, Tool } from '../src/agent.js';
import { MockLlmProvider } from '../src/llmProvider.js';

async function collect(gen: AsyncGenerator<StreamEvent>): Promise<StreamEvent[]> {
    const events: StreamEvent[] = [];
    for await (const e of gen) events.push(e);
    return events;
}

describe('SmoothAgent.runStream', () => {
    it('streams a multi-chunk text reply then exactly one done', async () => {
        const mock = new MockLlmProvider();
        mock.pushText('hello there friend, how are you', { prompt_tokens: 10, completion_tokens: 7 });

        const agent = new SmoothAgent(mock, {} satisfies AgentOptions);
        const events = await collect(agent.runStream('hi'));

        const textEvents = events.filter((e) => e.type === 'text');
        expect(textEvents.length).toBeGreaterThanOrEqual(2);
        const joined = textEvents.map((e) => (e as { text: string }).text).join('');
        expect(joined).toBe('hello there friend, how are you');

        const doneEvents = events.filter((e) => e.type === 'done');
        expect(doneEvents).toHaveLength(1);
        expect(events[events.length - 1].type).toBe('done');
        const done = doneEvents[0] as { response: { text: string } };
        expect(done.response.text).toBe('hello there friend, how are you');
    });

    it('round-trips a tool call: tool_call event, tool runs, tool_result, final answer', async () => {
        let ran = '';
        const echo: Tool = {
            name: 'echo',
            description: 'Echoes input',
            parameters: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] },
            execute: async (args) => {
                ran = String(args.text ?? '');
                return `echoed:${ran}`;
            },
        };
        const mock = new MockLlmProvider();
        mock.pushToolCall('call-1', 'echo', '{"text":"ping"}', { prompt_tokens: 5, completion_tokens: 3 });
        mock.pushText('all done', { prompt_tokens: 8, completion_tokens: 2 });

        const agent = new SmoothAgent(mock, { tools: [echo] } satisfies AgentOptions);
        const events = await collect(agent.runStream('use echo'));

        const toolCall = events.find((e) => e.type === 'tool_call') as { name: string; arguments: string } | undefined;
        expect(toolCall?.name).toBe('echo');
        expect(JSON.parse(toolCall!.arguments)).toEqual({ text: 'ping' });

        // The tool actually ran with the assembled args.
        expect(ran).toBe('ping');

        const toolResult = events.find((e) => e.type === 'tool_result') as { name: string; result: string } | undefined;
        expect(toolResult?.name).toBe('echo');
        expect(toolResult?.result).toBe('echoed:ping');

        const done = events.find((e) => e.type === 'done') as { response: { text: string; iterations: number; toolCalls: number } };
        expect(done.response.text).toBe('all done');
        expect(done.response.iterations).toBe(2);
        expect(done.response.toolCalls).toBe(1);
    });

    it('assembles tool-call arguments split across chunks before dispatch', async () => {
        let received: Record<string, unknown> | undefined;
        const tool: Tool = {
            name: 'save',
            description: 'Saves',
            parameters: { type: 'object', properties: { key: { type: 'string' }, value: { type: 'string' } } },
            execute: async (args) => {
                received = args;
                return 'saved';
            },
        };
        const mock = new MockLlmProvider();
        // The mock splits these arguments across two chunks; the agent must reassemble them.
        mock.pushToolCall('call-1', 'save', '{"key":"alpha","value":"beta-gamma-delta"}');
        mock.pushText('ok');

        const agent = new SmoothAgent(mock, { tools: [tool] } satisfies AgentOptions);
        await collect(agent.runStream('save it'));

        expect(received).toEqual({ key: 'alpha', value: 'beta-gamma-delta' });
    });

    it('done carries a response equivalent to run() for the same script', async () => {
        const script = (): MockLlmProvider => new MockLlmProvider().pushText('the answer is 42', { prompt_tokens: 12, completion_tokens: 6 });
        const opts: AgentOptions = { model: 'claude-haiku-4-5' };

        const runResult = await new SmoothAgent(script(), opts).run('q');
        const events = await collect(new SmoothAgent(script(), opts).runStream('q'));
        const done = events.find((e) => e.type === 'done') as { response: typeof runResult };

        expect(done.response.text).toBe(runResult.text);
        expect(done.response.iterations).toBe(runResult.iterations);
        expect(done.response.toolCalls).toBe(runResult.toolCalls);
        expect(done.response.usage).toEqual(runResult.usage);
        expect(done.response.costUsd).toBeCloseTo(runResult.costUsd, 10);
        expect(runResult.costUsd).toBeGreaterThan(0);
    });

    it('throws if the client cannot stream', async () => {
        const nonStreaming = {
            chat: { completions: { create: async () => ({ choices: [{ message: { content: 'x' } }] }) } },
        };
        const agent = new SmoothAgent(nonStreaming, {});
        await expect(collect(agent.runStream('hi'))).rejects.toThrow(/streaming-capable/);
    });
});
