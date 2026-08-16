/**
 * LLM request metadata + structured tool details — the TypeScript port of the
 * Rust reference's `ChatRequest.metadata` / `with_metadata` (LiteLLM spend-log
 * attribution) and `AgentEvent::ToolCallComplete.details` (UI-facing structured
 * tool payload attached by a postCall hook).
 */

import { describe, expect, it } from 'vitest';
import { AgentOptions, metadataField, SmoothAgent, StreamEvent, Tool, ToolHook } from '../src/agent.js';
import { MockLlmProvider } from '../src/llmProvider.js';

async function collect(gen: AsyncGenerator<StreamEvent>): Promise<StreamEvent[]> {
    const events: StreamEvent[] = [];
    for await (const e of gen) events.push(e);
    return events;
}

const echo: Tool = {
    name: 'echo',
    description: 'Echoes input',
    parameters: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] },
    execute: async (args) => `echoed:${String(args.text ?? '')}`,
};

describe('LLM request metadata', () => {
    it('is absent from the request body by default', async () => {
        const mock = new MockLlmProvider();
        mock.pushText('ok');
        const agent = new SmoothAgent(mock, {} satisfies AgentOptions);
        await agent.run('hi');
        expect('metadata' in mock.calls[0].body).toBe(false);
    });

    it('is forwarded verbatim as the top-level metadata object', async () => {
        const mock = new MockLlmProvider();
        mock.pushText('ok');
        const agent = new SmoothAgent(mock, { metadata: { smooai_agent_slug: 'support-bot', k: 'v' } } satisfies AgentOptions);
        await agent.run('hi');
        expect(mock.calls[0].body.metadata).toEqual({ smooai_agent_slug: 'support-bot', k: 'v' });
    });

    it('an empty metadata object is wire-identical to unset', async () => {
        const mock = new MockLlmProvider();
        mock.pushText('ok');
        const agent = new SmoothAgent(mock, { metadata: {} } satisfies AgentOptions);
        await agent.run('hi');
        expect('metadata' in mock.calls[0].body).toBe(false);
    });

    it('metadataField normalizes empty/absent to {}', () => {
        expect(metadataField(undefined)).toEqual({});
        expect(metadataField({})).toEqual({});
        expect(metadataField({ k: 'v' })).toEqual({ metadata: { k: 'v' } });
    });
});

describe('structured tool details', () => {
    it('forwards details attached by a postCall hook on the tool_result event', async () => {
        const details = { traceId: 'abc123', errorCount: 47 };
        const hook: ToolHook = {
            postCall: async (_call, result) => {
                result.details = details;
            },
        };
        const mock = new MockLlmProvider();
        mock.pushToolCall('call-1', 'echo', '{"text":"ping"}');
        mock.pushText('done');

        const agent = new SmoothAgent(mock, { tools: [echo], toolHooks: [hook] } satisfies AgentOptions);
        const events = await collect(agent.runStream('use echo'));

        const toolResult = events.find((e) => e.type === 'tool_result') as { result: string; details?: unknown } | undefined;
        expect(toolResult?.result).toBe('echoed:ping');
        expect(toolResult?.details).toEqual(details);
    });

    it('details is undefined when no hook attaches it', async () => {
        const mock = new MockLlmProvider();
        mock.pushToolCall('call-1', 'echo', '{"text":"ping"}');
        mock.pushText('done');

        const agent = new SmoothAgent(mock, { tools: [echo] } satisfies AgentOptions);
        const events = await collect(agent.runStream('use echo'));

        const toolResult = events.find((e) => e.type === 'tool_result') as { details?: unknown } | undefined;
        expect(toolResult?.details).toBeUndefined();
    });
});
