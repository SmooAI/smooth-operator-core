/**
 * Unit tests for human-in-the-loop approval (HumanGate).
 *
 * Driven by the same fake OpenAI-compatible client the other agent tests use, so
 * no credentials are needed. Covers the three behaviors: an approved tool
 * executes; a denied tool does NOT execute and its denial reason reaches the
 * model; and with no gate configured behavior is unchanged.
 */

import { describe, expect, it } from 'vitest';
import { AgentOptions, ChatClientLike, SmoothAgent, Tool } from '../src/agent.js';
import { approve, deny, HumanApprovalRequest, HumanDecision } from '../src/humanGate.js';

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

/** A tool that records every invocation so a test can assert it never ran. */
function spyTool(): { tool: Tool; invocations: Array<Record<string, unknown>> } {
    const invocations: Array<Record<string, unknown>> = [];
    const tool: Tool = {
        name: 'delete_record',
        description: 'Deletes a record (destructive).',
        parameters: { type: 'object', properties: { id: { type: 'string' } }, required: ['id'] },
        async execute(args) {
            invocations.push(args);
            return `deleted record ${String(args.id ?? '')}`;
        },
    };
    return { tool, invocations };
}

function toolCall(id: string, name: string, args: string): ScriptedMessage {
    return { content: null, tool_calls: [{ id, function: { name, arguments: args } }] };
}

describe('HumanGate', () => {
    it('approve/deny helpers', () => {
        expect(approve().decision).toBe(HumanDecision.Approved);
        const d = deny('nope');
        expect(d.decision).toBe(HumanDecision.Denied);
        expect(d.reason).toBe('nope');
    });

    it('executes a tool when the gate approves', async () => {
        const { tool, invocations } = spyTool();
        const seen: HumanApprovalRequest[] = [];
        const client = new FakeClient([toolCall('c1', 'delete_record', '{"id":"42"}'), { content: 'done' }]);
        const options: AgentOptions = {
            tools: [tool],
            humanGate: async (req) => {
                seen.push(req);
                return approve();
            },
            requiresApproval: (name) => name === 'delete_record',
        };
        const agent = new SmoothAgent(client, options);
        const result = await agent.run('delete record 42');

        expect(result.text).toBe('done');
        expect(result.toolCalls).toBe(1);
        // The gate saw the right request, and the tool actually ran.
        expect(seen).toHaveLength(1);
        expect(seen[0].toolName).toBe('delete_record');
        expect(seen[0].arguments).toEqual({ id: '42' });
        expect(invocations).toEqual([{ id: '42' }]);
        // The successful tool result was fed back to the model.
        const secondCallMessages = client.calls[1].messages as Array<Record<string, unknown>>;
        expect(secondCallMessages.some((m) => m.role === 'tool' && String(m.content).includes('deleted record 42'))).toBe(true);
    });

    it('does not execute a denied tool and feeds the denial reason to the model', async () => {
        const { tool, invocations } = spyTool();
        const client = new FakeClient([toolCall('c1', 'delete_record', '{"id":"42"}'), { content: "understood, I won't delete it" }]);
        const options: AgentOptions = {
            tools: [tool],
            humanGate: async () => deny('policy forbids deletes'),
            requiresApproval: (name) => name === 'delete_record',
        };
        const agent = new SmoothAgent(client, options);
        const result = await agent.run('delete record 42');

        // The tool never ran.
        expect(invocations).toEqual([]);
        expect(result.text).toBe("understood, I won't delete it");
        // The denial (with reason) was fed back to the model as the tool result.
        const secondCallMessages = client.calls[1].messages as Array<Record<string, unknown>>;
        const denial = secondCallMessages.find((m) => m.role === 'tool');
        expect(denial).toBeDefined();
        expect(String(denial!.content)).toContain('Denied by human');
        expect(String(denial!.content)).toContain('policy forbids deletes');
    });

    it('leaves behavior unchanged when no gate is configured', async () => {
        const { tool, invocations } = spyTool();
        const client = new FakeClient([toolCall('c1', 'delete_record', '{"id":"42"}'), { content: 'done' }]);
        // No humanGate set — even though requiresApproval matches, it is ignored.
        const agent = new SmoothAgent(client, { tools: [tool], requiresApproval: () => true });
        const result = await agent.run('delete record 42');

        expect(result.text).toBe('done');
        expect(invocations).toEqual([{ id: '42' }]);
    });

    it('only consults the gate for flagged tools', async () => {
        const { tool, invocations } = spyTool();
        const consulted: string[] = [];
        const client = new FakeClient([toolCall('c1', 'delete_record', '{"id":"7"}'), { content: 'done' }]);
        const agent = new SmoothAgent(client, {
            tools: [tool],
            humanGate: async (req) => {
                consulted.push(req.toolName);
                return deny('should not be asked');
            },
            // Flags a different tool, so this one runs without consulting the gate.
            requiresApproval: (name) => name === 'send_email',
        });
        const result = await agent.run('delete record 7');

        expect(consulted).toEqual([]);
        expect(invocations).toEqual([{ id: '7' }]);
        expect(result.text).toBe('done');
    });
});
