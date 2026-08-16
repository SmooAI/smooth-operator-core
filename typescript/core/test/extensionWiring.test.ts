/**
 * SEP extension-host wiring into the agent loop — the TypeScript sibling of
 * Rust's `Agent::with_extension_host` integration and Go's `extension_seam`
 * wiring tests (core #112). These cover ONLY the wiring that was previously
 * missing: the host's own fold policy and cross-tool guard are already tested
 * in `test/extension-*.test.ts`.
 */

import { describe, expect, it } from 'vitest';
import { AgentOptions, ExtensionFold, ExtensionHooks, SmoothAgent, StreamEvent, Tool } from '../src/agent.js';
import { ExtensionHost } from '../src/extension/host.js';
import { MockLlmProvider } from '../src/llmProvider.js';

// The concrete host must satisfy the structural seam — a compile-time assertion.
// If ExtensionHost's surface drifts from ExtensionHooks, this stops compiling.
type HostSatisfiesSeam = ExtensionHost extends ExtensionHooks ? true : never;
const _hostSatisfiesSeam: HostSatisfiesSeam = true;
void _hostSatisfiesSeam;

/** A fake host: canned tools, scripted tool_call verdicts, recorded events. */
class FakeHooks implements ExtensionHooks {
    events: Array<{ event: string; payload: unknown }> = [];
    hookCalls: Array<{ tool: string; args: unknown }> = [];
    verdict: (tool: string, args: unknown) => ExtensionFold = (_tool, args) => ({ kind: 'proceed', value: { arguments: args } });

    constructor(
        private readonly eager: Tool[] = [],
        private readonly deferred: Tool[] = [],
    ) {}

    tools(): Tool[] {
        return this.eager;
    }
    deferredTools(): Tool[] {
        return this.deferred;
    }
    async runToolCallHook(tool: string, args: unknown): Promise<ExtensionFold> {
        this.hookCalls.push({ tool, args });
        return this.verdict(tool, args);
    }
    dispatchEvent(event: string, payload: unknown): void {
        this.events.push({ event, payload });
    }
}

function extTool(name: string, ran: string[]): Tool {
    return {
        name,
        description: `extension tool ${name}`,
        parameters: { type: 'object', properties: { text: { type: 'string' } } },
        execute: async (args) => {
            ran.push(String(args.text ?? ''));
            return `ext:${String(args.text ?? '')}`;
        },
    };
}

async function collect(gen: AsyncGenerator<StreamEvent>): Promise<StreamEvent[]> {
    const events: StreamEvent[] = [];
    for await (const e of gen) events.push(e);
    return events;
}

describe('SEP extension wiring', () => {
    it('merges eager host tools as ordinary tools: visible to the model and dispatched', async () => {
        const ran: string[] = [];
        const hooks = new FakeHooks([extTool('demo.echo', ran)]);
        const mock = new MockLlmProvider();
        mock.pushToolCall('c1', 'demo.echo', '{"text":"hi"}');
        mock.pushText('done');

        const agent = new SmoothAgent(mock, { extensions: hooks } satisfies AgentOptions);
        const result = await agent.run('use the extension');

        expect(ran).toEqual(['hi']); // the extension tool actually executed
        expect(result.text).toBe('done');
        // The tool schema was advertised to the model.
        expect(mock.calls[0].tools?.some((t) => (t as { function?: { name?: string } }).function?.name === 'demo.echo')).toBe(true);
    });

    it('deferred host tools stay HIDDEN until tool_search promotes them', async () => {
        const ran: string[] = [];
        const hooks = new FakeHooks([], [extTool('demo.rare', ran)]);
        const mock = new MockLlmProvider();
        // The model tries to call the deferred tool directly, unpromoted.
        mock.pushToolCall('c1', 'demo.rare', '{"text":"sneak"}');
        mock.pushText('done');

        const agent = new SmoothAgent(mock, { extensions: hooks } satisfies AgentOptions);
        await agent.run('try it');

        expect(ran).toEqual([]); // never executed — invisible until promoted
        // Its schema was NOT advertised; only tool_search was.
        const advertised = (mock.calls[0].tools ?? []).map((t) => (t as { function?: { name?: string } }).function?.name);
        expect(advertised).not.toContain('demo.rare');
        expect(advertised).toContain('tool_search');
    });

    it('a blocked tool_call verdict vetoes execution and surfaces the reason to the model', async () => {
        const ran: string[] = [];
        const hooks = new FakeHooks([extTool('demo.echo', ran)]);
        hooks.verdict = () => ({ kind: 'blocked', reason: 'policy says no' });
        const mock = new MockLlmProvider();
        mock.pushToolCall('c1', 'demo.echo', '{"text":"hi"}');
        mock.pushText('done');

        const agent = new SmoothAgent(mock, { extensions: hooks } satisfies AgentOptions);
        await agent.run('use it');

        expect(ran).toEqual([]); // vetoed calls never reach dispatch
        // The veto reason became the tool result the model saw on the next call.
        const toolMsg = mock.calls[1].messages.find((m) => m.role === 'tool');
        expect(toolMsg?.content).toBe('error: blocked by extension: policy says no');
    });

    it('a proceed verdict with rewritten arguments runs the tool with the patch', async () => {
        const ran: string[] = [];
        const hooks = new FakeHooks([extTool('demo.echo', ran)]);
        hooks.verdict = () => ({ kind: 'proceed', value: { arguments: { text: 'patched' } } });
        const mock = new MockLlmProvider();
        mock.pushToolCall('c1', 'demo.echo', '{"text":"original"}');
        mock.pushText('done');

        const agent = new SmoothAgent(mock, { extensions: hooks } satisfies AgentOptions);
        await agent.run('use it');

        expect(ran).toEqual(['patched']);
        expect(hooks.hookCalls).toEqual([{ tool: 'demo.echo', args: { text: 'original' } }]);
    });

    it('emits turn_start, then message_end + turn_end in order, with payloads', async () => {
        const hooks = new FakeHooks();
        const mock = new MockLlmProvider();
        mock.pushText('final answer');

        const agent = new SmoothAgent(mock, { extensions: hooks, model: 'test-model' } satisfies AgentOptions);
        await agent.run('hello');

        expect(hooks.events.map((e) => e.event)).toEqual(['turn_start', 'message_end', 'turn_end']);
        expect(hooks.events[0].payload).toEqual({ agent_id: 'test-model' });
        expect(hooks.events[1].payload).toEqual({ iteration: 1, content: 'final answer' });
        expect(hooks.events[2].payload).toEqual({ agent_id: 'test-model', iterations: 1 });
    });

    it('emits the end events on the budget-exceeded exit too', async () => {
        const hooks = new FakeHooks();
        const mock = new MockLlmProvider();
        mock.pushText('partial', { prompt_tokens: 900, completion_tokens: 900 });

        const agent = new SmoothAgent(mock, { extensions: hooks, budget: { maxTokens: 10 } } satisfies AgentOptions);
        const result = await agent.run('hello');

        expect(result.budgetExceeded).toBe(true);
        expect(hooks.events.map((e) => e.event)).toEqual(['turn_start', 'message_end', 'turn_end']);
    });

    it('wires runStream identically: events fire and vetoes surface on the stream', async () => {
        const ran: string[] = [];
        const hooks = new FakeHooks([extTool('demo.echo', ran)]);
        hooks.verdict = () => ({ kind: 'blocked', reason: 'stream veto' });
        const mock = new MockLlmProvider();
        mock.pushToolCall('c1', 'demo.echo', '{"text":"hi"}');
        mock.pushText('done');

        const agent = new SmoothAgent(mock, { extensions: hooks } satisfies AgentOptions);
        const events = await collect(agent.runStream('use it'));

        expect(ran).toEqual([]);
        const toolResult = events.find((e) => e.type === 'tool_result') as { result: string } | undefined;
        expect(toolResult?.result).toBe('error: blocked by extension: stream veto');
        expect(hooks.events.map((e) => e.event)).toEqual(['turn_start', 'message_end', 'turn_end']);
    });

    it('no host configured: loop behaves exactly as before (no events, tools untouched)', async () => {
        const mock = new MockLlmProvider();
        mock.pushText('plain');
        const agent = new SmoothAgent(mock, {} satisfies AgentOptions);
        const result = await agent.run('hello');
        expect(result.text).toBe('plain');
    });
});
