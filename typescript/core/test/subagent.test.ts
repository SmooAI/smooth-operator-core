import { describe, expect, it } from 'vitest';
import { ChatClientLike, delegateTool, SmoothAgent } from '../src/agent.js';

function fake(scripted: Array<{ content: string | null; tool_calls?: unknown }>): ChatClientLike {
    let i = 0;
    return {
        chat: {
            completions: {
                create: async () => {
                    const s = scripted[i++];
                    return { choices: [{ message: { content: s.content, tool_calls: s.tool_calls as never } }] };
                },
            },
        },
    };
}

describe('sub-agent delegation', () => {
    it('runs the child agent and returns its reply as the tool result', async () => {
        const child = new SmoothAgent(fake([{ content: 'researched: 42' }]), { instructions: 'researcher' });
        const researcher = delegateTool('researcher', 'Delegate a research subtask.', child);

        const parent = new SmoothAgent(
            fake([
                { content: null, tool_calls: [{ id: 'c1', function: { name: 'researcher', arguments: '{"task":"find the answer"}' } }] },
                { content: 'the answer is 42' },
            ]),
            { tools: [researcher] },
        );

        const result = await parent.run('delegate to the researcher');
        expect(result.text).toBe('the answer is 42');
        expect(result.toolCalls).toBe(1);
    });

    it('exposes a task-requiring schema', () => {
        const child = new SmoothAgent(fake([{ content: 'x' }]), {});
        const tool = delegateTool('helper', 'help', child);
        expect(tool.name).toBe('helper');
        const params = tool.parameters as { properties: Record<string, unknown>; required: string[] };
        expect(params.properties).toHaveProperty('task');
        expect(params.required).toEqual(['task']);
    });
});
