/**
 * Tests for the `tool_search` meta-tool and deferred-tool promotion.
 *
 * Proves the Phase-3 behaviour: a deferred tool's schema is NOT sent to the model
 * until it's promoted; `tool_search` fuzzy-matches the query and promotes matches;
 * a promoted tool is then dispatchable; an unpromoted deferred tool is not.
 */

import { describe, expect, it } from 'vitest';
import { AgentOptions, ChatClientLike, SmoothAgent, Tool } from '../src/agent.js';
import { ToolSearch } from '../src/toolSearch.js';

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

function funcTool(name: string, description: string): Tool {
    return {
        name,
        description,
        parameters: { type: 'object' },
        execute: async () => `ran ${name}`,
    };
}

function specNames(call: Record<string, unknown>): string[] {
    const tools = (call.tools as Array<{ function: { name: string } }> | undefined) ?? [];
    return tools.map((t) => t.function.name);
}

function toolMsg(id: string, name: string, args: string) {
    return { id, function: { name, arguments: args } };
}

describe('ToolSearch (unit)', () => {
    it('matches by name and promotes the matches', async () => {
        const search = new ToolSearch([funcTool('git_status', 'Show git working tree status'), funcTool('git_diff', 'Show git diff between commits'), funcTool('http_get', 'Fetch a URL via HTTP GET')]);
        const out = JSON.parse(await search.execute({ query: 'git' }));
        expect(out.matched).toBe(2);
        expect(new Set(out.tools.map((t: { name: string }) => t.name))).toEqual(new Set(['git_status', 'git_diff']));
        expect(search.isPromoted('git_status')).toBe(true);
        expect(search.isPromoted('git_diff')).toBe(true);
        expect(search.isPromoted('http_get')).toBe(false);
    });

    it('matches by description, case-insensitively', async () => {
        const search = new ToolSearch([funcTool('http_get', 'Fetch a URL via HTTP GET')]);
        const out = JSON.parse(await search.execute({ query: 'url' }));
        expect(out.matched).toBe(1);
        expect(search.isPromoted('http_get')).toBe(true);
    });

    it('promotes nothing on no match', async () => {
        const search = new ToolSearch([funcTool('git_status', 'Show git status')]);
        const out = JSON.parse(await search.execute({ query: 'xyzzy' }));
        expect(out.matched).toBe(0);
        expect(out.tools).toHaveLength(0);
        expect(search.isPromoted('git_status')).toBe(false);
    });

    it('treats an empty query as a no-op', async () => {
        const search = new ToolSearch([funcTool('git_status', 'Show git status')]);
        const out = JSON.parse(await search.execute({ query: '   ' }));
        expect(out.matched).toBe(0);
        expect(search.isPromoted('git_status')).toBe(false);
    });
});

describe('SmoothAgent deferred tools', () => {
    it('hides deferred schemas until promoted, then dispatches the promoted tool', async () => {
        const gitStatus = funcTool('git_status', 'Show git working tree status');
        const httpGet = funcTool('http_get', 'Fetch a URL via HTTP GET');
        const eager = funcTool('echo', 'Echo back');

        const client = new FakeClient([
            { content: null, tool_calls: [toolMsg('c1', 'tool_search', '{"query": "git"}')] },
            { content: null, tool_calls: [toolMsg('c2', 'git_status', '{}')] },
            { content: 'done' },
        ]);
        const opts: AgentOptions = { tools: [eager], deferredTools: [gitStatus, httpGet] };
        const agent = new SmoothAgent(client, opts);
        const result = await agent.run('inspect the repo');
        expect(result.text).toBe('done');
        expect(result.toolCalls).toBe(2);

        // Turn 1: eager tool + tool_search advertised; deferred tools hidden.
        const first = specNames(client.calls[0]);
        expect(first).toContain('echo');
        expect(first).toContain('tool_search');
        expect(first).not.toContain('git_status');
        expect(first).not.toContain('http_get');
        // Turn 2: git_status promoted into view; http_get still hidden.
        const second = specNames(client.calls[1]);
        expect(second).toContain('git_status');
        expect(second).not.toContain('http_get');
        // The promoted tool actually dispatched (ran), fed back as a tool message.
        const secondMessages = client.calls[1].messages as Array<Record<string, unknown>>;
        expect(secondMessages.some((m) => m.role === 'tool' && m.content === 'ran git_status')).toBe(true);
    });

    it('does not dispatch an unpromoted deferred tool', async () => {
        const gitStatus = funcTool('git_status', 'Show git working tree status');
        const client = new FakeClient([{ content: null, tool_calls: [toolMsg('c1', 'git_status', '{}')] }, { content: 'ok' }]);
        const agent = new SmoothAgent(client, { deferredTools: [gitStatus] });
        const result = await agent.run('try it');
        expect(result.text).toBe('ok');
        const toolMsgs = (client.calls[1].messages as Array<Record<string, unknown>>).filter((m) => m.role === 'tool');
        expect(toolMsgs).not.toHaveLength(0);
        expect(toolMsgs[0].content).toContain("unknown tool 'git_status'");
    });

    it('does not advertise tool_search when there are no deferred tools', async () => {
        const client = new FakeClient([{ content: 'hi' }]);
        const agent = new SmoothAgent(client, { tools: [funcTool('echo', 'echo')] });
        await agent.run('hello');
        expect(specNames(client.calls[0])).not.toContain('tool_search');
    });
});
