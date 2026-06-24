import { describe, expect, it } from 'vitest';
import { ChatClientLike, SmoothAgent, Tool } from '../src/agent.js';
import { Cast, Clearance, makeRole, RoleKind } from '../src/cast.js';

describe('Clearance', () => {
    it('empty allow + empty deny allows all tools', () => {
        const c = Clearance.allowAll();
        expect(c.isAllowed('anything')).toBe(true);
        expect(c.isAllowed('other')).toBe(true);
    });

    it('denyAll blocks every tool, even allow-listed ones', () => {
        expect(Clearance.denyAll().isAllowed('anything')).toBe(false);
        expect(new Clearance({ allowTools: ['x'], denyEverything: true }).isAllowed('x')).toBe(false);
    });

    it('a non-empty allow-list is a whitelist', () => {
        const c = Clearance.allow('read', 'search');
        expect(c.isAllowed('read')).toBe(true);
        expect(c.isAllowed('search')).toBe(true);
        expect(c.isAllowed('write')).toBe(false);
    });

    it('deny always wins over allow', () => {
        const c = new Clearance({ allowTools: ['read', 'write'], denyTools: ['write'] });
        expect(c.isAllowed('read')).toBe(true);
        expect(c.isAllowed('write')).toBe(false);
    });

    it('a deny-list with empty allow blocks only the denied tools', () => {
        const c = Clearance.deny('delete');
        expect(c.isAllowed('delete')).toBe(false);
        expect(c.isAllowed('read')).toBe(true);
    });
});

describe('Cast', () => {
    it('registers, gets, lists, and filters roles', () => {
        const cast = new Cast();
        const lead = makeRole('lead', RoleKind.Lead, { instructions: 'orchestrate' });
        const sk = makeRole('researcher', RoleKind.Sidekick, { instructions: 'research' });
        const shadow = makeRole('critic', RoleKind.Shadow, { instructions: 'observe', hidden: true });
        cast.register(lead).register(sk).register(shadow);

        expect(cast.count).toBe(3);
        expect(cast.isEmpty).toBe(false);
        expect(cast.get('researcher')).toBe(sk);
        expect(cast.get('missing')).toBeUndefined();
        expect(cast.sidekicks()).toEqual([sk]);
        expect(cast.listVisible().map((r) => r.name).sort()).toEqual(['lead', 'researcher']);
    });

    it('makeRole defaults to allow-all clearance and 8 iterations', () => {
        const role = makeRole('lead', RoleKind.Lead);
        expect(role.permissions.isAllowed('any-tool')).toBe(true);
        expect(role.maxIterations).toBe(8);
    });
});

// ── Agent enforcement ────────────────────────────────────────────────────────
function fake(
    scripted: Array<{ content: string | null; tool_calls?: Array<{ id: string; function: { name: string; arguments: string } }> }>,
): { client: ChatClientLike; calls: Array<Record<string, unknown>> } {
    const calls: Array<Record<string, unknown>> = [];
    let i = 0;
    const client: ChatClientLike = {
        chat: {
            completions: {
                create: async (body) => {
                    calls.push(body);
                    const s = scripted[i++];
                    return { choices: [{ message: { content: s.content, tool_calls: s.tool_calls ?? null } }] };
                },
            },
        },
    };
    return { client, calls };
}

function spyTool(name: string, executed: string[]): Tool {
    return {
        name,
        description: `the ${name} tool`,
        parameters: { type: 'object', properties: {} },
        async execute() {
            executed.push(name);
            return `${name} ran`;
        },
    };
}

describe('SmoothAgent clearance enforcement', () => {
    it('does not execute a forbidden tool and tells the model', async () => {
        const executed: string[] = [];
        const { client, calls } = fake([
            { content: null, tool_calls: [{ id: 'c1', function: { name: 'write', arguments: '{}' } }] },
            { content: "ok, I won't write" },
        ]);
        const agent = new SmoothAgent(client, { tools: [spyTool('write', executed)], clearance: Clearance.deny('write') });
        const res = await agent.run('please write');

        expect(res.text).toBe("ok, I won't write");
        expect(res.toolCalls).toBe(1); // counted...
        expect(executed).toEqual([]); // ...but the body never ran.
        const secondMessages = calls[1].messages as Array<Record<string, unknown>>;
        expect(secondMessages.some((m) => m.role === 'tool' && String(m.content).includes('not permitted'))).toBe(true);
    });

    it('still runs an allowed tool under a whitelist clearance', async () => {
        const executed: string[] = [];
        const { client } = fake([
            { content: null, tool_calls: [{ id: 'c1', function: { name: 'read', arguments: '{}' } }] },
            { content: 'done' },
        ]);
        const agent = new SmoothAgent(client, { tools: [spyTool('read', executed)], clearance: Clearance.allow('read') });
        const res = await agent.run('please read');
        expect(res.text).toBe('done');
        expect(executed).toEqual(['read']);
    });

    it('allows every tool when no clearance is set', async () => {
        const executed: string[] = [];
        const { client } = fake([
            { content: null, tool_calls: [{ id: 'c1', function: { name: 'write', arguments: '{}' } }] },
            { content: 'done' },
        ]);
        const agent = new SmoothAgent(client, { tools: [spyTool('write', executed)] });
        await agent.run('please write');
        expect(executed).toEqual(['write']);
    });
});
