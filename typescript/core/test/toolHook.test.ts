/**
 * Unit tests for the tool-call surveillance lifecycle (ToolHook).
 *
 * Mirrors the Rust reference engine's `ToolHook` tests (`tool.rs`): a spy hook's
 * `preCall`/`postCall` fire around a dispatched tool; a `preCall` that throws
 * blocks the call (the tool never runs, the model is told); and a `postCall` that
 * rewrites `result.content` redacts what the model/conversation sees. Driven by
 * the same fake OpenAI-compatible client the other agent tests use — no creds.
 */

import { describe, expect, it } from 'vitest';
import { AgentOptions, ChatClientLike, SmoothAgent, Tool, ToolCall, ToolHook, ToolResult } from '../src/agent.js';

type ScriptedMessage = {
    content: string | null;
    tool_calls?: Array<{ id: string; function: { name: string; arguments: string } }> | null;
};

class FakeClient implements ChatClientLike {
    readonly calls: Array<Record<string, unknown>> = [];
    private readonly scripted: ScriptedMessage[];

    constructor(scripted: ScriptedMessage[]) {
        this.scripted = [...scripted];
    }

    chat = {
        completions: {
            create: async (body: Record<string, unknown>) => {
                this.calls.push(body);
                const message = this.scripted.shift()!;
                return { choices: [{ message }] };
            },
        },
    };
}

/** A tool that records every invocation so a test can assert whether it ran. */
function spyTool(): { tool: Tool; invocations: Array<Record<string, unknown>> } {
    const invocations: Array<Record<string, unknown>> = [];
    const tool: Tool = {
        name: 'echo',
        description: 'Echoes its text back.',
        parameters: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] },
        async execute(args) {
            invocations.push(args);
            return `echoed:${String(args.text ?? '')}`;
        },
    };
    return { tool, invocations };
}

function toolCall(id: string, name: string, args: string): ScriptedMessage {
    return { content: null, tool_calls: [{ id, function: { name, arguments: args } }] };
}

/** Read the tool-result message content fed back to the model on the 2nd model call. */
function toolResultContent(client: FakeClient): string {
    const secondCallMessages = client.calls[1].messages as Array<Record<string, unknown>>;
    const msg = secondCallMessages.find((m) => m.role === 'tool');
    return String(msg?.content ?? '');
}

describe('ToolHook', () => {
    it('fires preCall then postCall around a dispatched tool', async () => {
        const { tool, invocations } = spyTool();
        const preSeen: ToolCall[] = [];
        const postSeen: Array<{ call: ToolCall; result: ToolResult }> = [];
        const hook: ToolHook = {
            async preCall(call) {
                preSeen.push({ ...call });
            },
            async postCall(call, result) {
                // Snapshot by value — the same object may be mutated later.
                postSeen.push({ call: { ...call }, result: { ...result } });
            },
        };
        const client = new FakeClient([toolCall('c1', 'echo', '{"text":"hi"}'), { content: 'done' }]);
        const options: AgentOptions = { tools: [tool], toolHooks: [hook] };
        const agent = new SmoothAgent(client, options);
        const result = await agent.run('say hi');

        expect(result.text).toBe('done');
        // The tool actually ran.
        expect(invocations).toEqual([{ text: 'hi' }]);
        // preCall saw the call (with id + parsed args) before execution.
        expect(preSeen).toHaveLength(1);
        expect(preSeen[0]).toEqual({ id: 'c1', name: 'echo', arguments: { text: 'hi' } });
        // postCall saw the successful, non-error result.
        expect(postSeen).toHaveLength(1);
        expect(postSeen[0].call.id).toBe('c1');
        expect(postSeen[0].result.toolCallId).toBe('c1');
        expect(postSeen[0].result.isError).toBe(false);
        expect(postSeen[0].result.content).toBe('echoed:hi');
    });

    it('a preCall that throws blocks the tool and tells the model', async () => {
        const { tool, invocations } = spyTool();
        const hook: ToolHook = {
            async preCall(call) {
                if (call.name === 'echo') throw new Error('blocked by policy');
            },
        };
        const client = new FakeClient([toolCall('c1', 'echo', '{"text":"hi"}'), { content: 'ok, blocked' }]);
        const agent = new SmoothAgent(client, { tools: [tool], toolHooks: [hook] });
        const result = await agent.run('say hi');

        // The tool never ran.
        expect(invocations).toEqual([]);
        expect(result.text).toBe('ok, blocked');
        // The block reason was fed back to the model as the tool result.
        const content = toolResultContent(client);
        expect(content).toContain('blocked by hook');
        expect(content).toContain('blocked by policy');
    });

    it('a postCall mutation redacts the result the model sees', async () => {
        const { tool } = spyTool();
        // The echo tool returns `echoed:the secret is 1234`; the hook redacts it.
        const redactHook: ToolHook = {
            async postCall(_call, result) {
                result.content = result.content.replace(/secret is \d+/, 'secret is [REDACTED]');
            },
        };
        const client = new FakeClient([toolCall('c1', 'echo', '{"text":"the secret is 1234"}'), { content: 'done' }]);
        const agent = new SmoothAgent(client, { tools: [tool], toolHooks: [redactHook] });
        await agent.run('echo the secret');

        const content = toolResultContent(client);
        expect(content).toBe('echoed:the secret is [REDACTED]');
        expect(content).not.toContain('1234');
    });

    it('addHook appends a hook after construction', async () => {
        const { tool } = spyTool();
        const fired: string[] = [];
        const client = new FakeClient([toolCall('c1', 'echo', '{"text":"hi"}'), { content: 'done' }]);
        const agent = new SmoothAgent(client, { tools: [tool] });
        agent.addHook({
            async preCall(call) {
                fired.push(`pre:${call.name}`);
            },
            async postCall(call) {
                fired.push(`post:${call.name}`);
            },
        });
        await agent.run('say hi');
        expect(fired).toEqual(['pre:echo', 'post:echo']);
    });

    it('a throwing postCall is swallowed — the result still reaches the caller', async () => {
        const { tool } = spyTool();
        const client = new FakeClient([toolCall('c1', 'echo', '{"text":"hi"}'), { content: 'done' }]);
        const agent = new SmoothAgent(client, {
            tools: [tool],
            toolHooks: [
                {
                    async postCall() {
                        throw new Error('hook exploded');
                    },
                },
            ],
        });
        const result = await agent.run('say hi');
        // The turn completed despite the post-hook throwing.
        expect(result.text).toBe('done');
        expect(toolResultContent(client)).toBe('echoed:hi');
    });

    it('hooks run in order (construction hooks before addHook)', async () => {
        const { tool } = spyTool();
        const order: string[] = [];
        const client = new FakeClient([toolCall('c1', 'echo', '{"text":"hi"}'), { content: 'done' }]);
        const agent = new SmoothAgent(client, {
            tools: [tool],
            toolHooks: [{ async preCall() { order.push('first'); } }],
        });
        agent.addHook({ async preCall() { order.push('second'); } });
        await agent.run('say hi');
        expect(order).toEqual(['first', 'second']);
    });
});
