import { describe, expect, it } from 'vitest';
import { ChatClientLike, SmoothAgent } from '../src/agent.js';
import { InMemoryMemory } from '../src/memory.js';

describe('memory', () => {
    it('remembers and recalls by overlap', () => {
        const mem = new InMemoryMemory();
        mem.remember("The user's name is Dana.");
        mem.remember('The user prefers metric units.');
        mem.remember('Gift wrapping costs 4.99.');
        const recalled = mem.recall('what units does the user prefer?', 1);
        expect(recalled).toHaveLength(1);
        expect(recalled[0].text).toContain('metric');
    });

    it('returns nothing on no overlap', () => {
        const mem = new InMemoryMemory();
        mem.remember('The sky is blue.');
        expect(mem.recall('quarterly revenue forecast', 4)).toEqual([]);
    });

    it('ignores blank memories', () => {
        const mem = new InMemoryMemory();
        mem.remember('   ');
        expect(mem.recall('anything', 4)).toEqual([]);
    });

    it('injects recalled memory into the system prompt', async () => {
        const mem = new InMemoryMemory();
        mem.remember("The user's name is Dana.");
        mem.remember('Unrelated trivia about penguins.');

        let systemContent = '';
        const client: ChatClientLike = {
            chat: {
                completions: {
                    create: async (body: Record<string, unknown>) => {
                        const messages = body.messages as Array<Record<string, unknown>>;
                        systemContent = (messages[0].content as string) ?? '';
                        return { choices: [{ message: { content: 'Hi Dana!' } }] };
                    },
                },
            },
        };
        const agent = new SmoothAgent(client, { instructions: 'support', memory: mem });
        await agent.run('do you remember my name?');
        expect(systemContent).toContain('Relevant memory');
        expect(systemContent).toContain('Dana');
        expect(systemContent).not.toContain('penguins');
    });
});
