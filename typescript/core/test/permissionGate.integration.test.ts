/**
 * Integration: the permission gate wired into a real {@link SmoothAgent} run.
 * Proves a denied tool is NOT executed and the reason reaches the model, an
 * allowed tool runs, and — the key additive-no-op guarantee — with no permission
 * option set the gate is off and behaviour is unchanged. Ports the spirit of the
 * Rust engine's `hook_gates_registry_execution` test.
 */

import { describe, expect, it } from 'vitest';
import { ChatClientLike, SmoothAgent, Tool } from '../src/agent.js';
import { AutoMode } from '../src/permission.js';
import { approve, HumanGate } from '../src/humanGate.js';

type ScriptedMessage = {
    content: string | null;
    tool_calls?: Array<{ id: string; function: { name: string; arguments: string } }> | null;
};

/** A fake OpenAI client that replays a script and records the tool-result messages it received. */
class FakeClient implements ChatClientLike {
    readonly toolResults: string[] = [];
    private readonly scripted: ScriptedMessage[];

    constructor(scripted: ScriptedMessage[]) {
        this.scripted = [...scripted];
    }

    chat = {
        completions: {
            create: async (body: Record<string, unknown>) => {
                // Capture any tool-result messages so a test can assert what the model saw.
                const msgs = (body.messages as Array<{ role: string; content: string }>) ?? [];
                for (const m of msgs) if (m.role === 'tool') this.toolResults.push(m.content);
                const message = this.scripted.shift()!;
                return { choices: [{ message }] };
            },
        },
    };
}

/** A tool that records every invocation so a test can assert it never ran. */
function spyBash(): { tool: Tool; runs: number[] } {
    const runs: number[] = [];
    const tool: Tool = {
        name: 'bash',
        description: 'run a shell command',
        parameters: { type: 'object', properties: { cmd: { type: 'string' } } },
        async execute() {
            runs.push(1);
            return 'ran';
        },
    };
    return { tool, runs };
}

const toolCallMsg = (cmd: string): ScriptedMessage => ({
    content: null,
    tool_calls: [{ id: 't1', function: { name: 'bash', arguments: JSON.stringify({ cmd }) } }],
});

describe('permission gate integration', () => {
    it('denies a circuit-breaker before the tool runs; the model sees the reason', async () => {
        const { tool, runs } = spyBash();
        const client = new FakeClient([toolCallMsg('rm -rf /'), { content: 'ok, aborting' }]);
        const agent = new SmoothAgent(client, { tools: [tool], permissionMode: AutoMode.Bypass, maxIterations: 3 });
        await agent.run('clean up');
        expect(runs.length).toBe(0);
        expect(client.toolResults.some((r) => /blocked by permission policy/.test(r))).toBe(true);
    });

    it('runs an allowed read-only command', async () => {
        const { tool, runs } = spyBash();
        const client = new FakeClient([toolCallMsg('ls -la'), { content: 'done' }]);
        const agent = new SmoothAgent(client, { tools: [tool], permissionMode: AutoMode.Ask, maxIterations: 3 });
        const res = await agent.run('list files');
        expect(runs.length).toBe(1);
        expect(res.text).toBe('done');
    });

    it('routes an Ask to the humanGate; approval lets it run', async () => {
        const { tool, runs } = spyBash();
        const gate: HumanGate = async () => approve();
        const client = new FakeClient([toolCallMsg('npm install left-pad'), { content: 'installed' }]);
        const agent = new SmoothAgent(client, { tools: [tool], permissionMode: AutoMode.Ask, humanGate: gate, maxIterations: 3 });
        await agent.run('install a dep');
        expect(runs.length).toBe(1);
    });

    it('fails closed on an Ask with no humanGate; tool never runs', async () => {
        const { tool, runs } = spyBash();
        const client = new FakeClient([toolCallMsg('npm install left-pad'), { content: 'ok' }]);
        const agent = new SmoothAgent(client, { tools: [tool], permissionMode: AutoMode.Ask, maxIterations: 3 });
        await agent.run('install a dep');
        expect(runs.length).toBe(0);
        expect(client.toolResults.some((r) => /fail-closed/.test(r))).toBe(true);
    });

    it('additive no-op: with no permission option the gate is OFF (a mutating call runs)', async () => {
        const { tool, runs } = spyBash();
        const client = new FakeClient([toolCallMsg('npm install left-pad'), { content: 'ok' }]);
        const agent = new SmoothAgent(client, { tools: [tool], maxIterations: 3 });
        await agent.run('install a dep');
        expect(runs.length).toBe(1);
    });
});
