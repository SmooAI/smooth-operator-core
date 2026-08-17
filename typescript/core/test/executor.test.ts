/**
 * Unit tests for the durable-execution seam (ADR-030), mirroring the Rust
 * reference's `executor.rs` / `activities.rs` tests one-for-one so the parity is
 * visible: the in-process executor is a verbatim delegation to the agent's own
 * run entry points, and `driveTurn` reproduces the loop's decision flow.
 */

import { describe, expect, it } from 'vitest';
import { SmoothAgent } from '../src/agent.js';
import type { StreamEvent, Tool } from '../src/agent.js';
import { driveTurn, InProcessActivities, InProcessExecutor } from '../src/executor.js';
import { MockLlmProvider } from '../src/llmProvider.js';

/** Minimal tool that echoes its `text` argument — mirrors the Rust tests' `EchoTool`. */
const echoTool: Tool = {
    name: 'echo',
    description: 'Echoes input back',
    parameters: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] },
    async execute(args) {
        return String(args.text ?? '');
    },
};

/** The seed state `SmoothAgent.run` holds when it enters its loop. */
function seedMessages(user: string): Array<Record<string, unknown>> {
    return [
        { role: 'system', content: 'You are a test agent' },
        { role: 'user', content: user },
    ];
}

describe('InProcessExecutor', () => {
    it('produces the same result as calling the agent run directly', async () => {
        const mock = new MockLlmProvider();
        mock.pushText('the answer is 42');
        const agent = new SmoothAgent(mock, { instructions: 'You are a test agent' });

        const result = await new InProcessExecutor().execute(agent, 'what is the answer?');

        expect(result.text).toBe('the answer is 42');
        expect(result.iterations).toBe(1);
        expect(mock.callCount).toBe(1);
        expect(JSON.stringify(mock.calls[0].messages)).toContain('what is the answer?');
    });

    it('streaming emits events and returns the same final result', async () => {
        const mock = new MockLlmProvider();
        mock.pushText('streamed reply');
        const agent = new SmoothAgent(mock, { instructions: 'You are a test agent' });

        const events: StreamEvent[] = [];
        for await (const event of new InProcessExecutor().executeStreaming(agent, 'stream please')) {
            events.push(event);
        }

        expect(events.length).toBeGreaterThan(1);
        expect(events.filter((e) => e.type === 'text').length).toBeGreaterThan(0);
        const done = events[events.length - 1];
        expect(done.type).toBe('done');
        expect(done.type === 'done' && done.response.text).toBe('streamed reply');
    });
});

describe('driveTurn', () => {
    it('stops after exactly one model call on a plain text reply', async () => {
        const mock = new MockLlmProvider();
        mock.pushText('the answer is 42');
        const activities = new InProcessActivities(mock);

        const messages = seedMessages('what is the answer?');
        await driveTurn(activities, messages, []);

        expect(mock.callCount).toBe(1);
        expect(messages[messages.length - 1]).toEqual({ role: 'assistant', content: 'the answer is 42' });
    });

    it('runs the tool, appends the result paired to the call, and loops back to the model', async () => {
        const mock = new MockLlmProvider();
        mock.pushToolCall('call-1', 'echo', JSON.stringify({ text: 'hello tools' }));
        mock.pushText('done');
        const activities = new InProcessActivities(mock, [echoTool]);

        const messages = seedMessages('use the echo tool');
        await driveTurn(activities, messages, []);

        expect(mock.callCount).toBe(2);
        const toolMsgs = messages.filter((m) => m.role === 'tool');
        expect(toolMsgs).toEqual([{ role: 'tool', tool_call_id: 'call-1', name: 'echo', content: 'hello tools' }]);
        expect(messages[messages.length - 1]).toEqual({ role: 'assistant', content: 'done' });
    });

    it('bounds the loop at maxIterations', async () => {
        const mock = new MockLlmProvider();
        for (let i = 0; i < 5; i++) mock.pushToolCall(`call-${i}`, 'echo', JSON.stringify({ text: 'again' }));
        const activities = new InProcessActivities(mock, [echoTool]);

        const messages = seedMessages('loop forever');
        await driveTurn(activities, messages, [], { maxIterations: 2 });

        expect(mock.callCount).toBe(2);
        expect(messages.filter((m) => m.role === 'tool')).toHaveLength(2);
    });
});
