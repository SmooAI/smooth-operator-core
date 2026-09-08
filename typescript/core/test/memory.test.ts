import { describe, expect, it } from 'vitest';
import { ChatClientLike, SmoothAgent } from '../src/agent.js';
import {
    InMemoryMemory,
    RECALL_FRESHNESS_NOTE,
    RECALL_HEADER,
    relevanceScore,
    renderRecallBlock,
} from '../src/memory.js';

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
        expect(systemContent).toContain(RECALL_HEADER);
        expect(systemContent).toContain('Dana');
        expect(systemContent).not.toContain('penguins');
    });
});

// ── cross-language recall contract (th-ffaeae) ───────────────────────────────
//
// The block below is reproduced byte-for-byte by the Rust reference and the C#,
// Python and Go siblings. These tests mirror rust/smooth-operator-core/src/memory.rs
// so a drift shows up here, not months later when someone tries to write a shared
// conformance scenario.

describe('recall block (cross-language contract)', () => {
    it('is pinned across languages', () => {
        expect(renderRecallBlock([{ text: 'brent prefers execution over questions', memoryType: 'User', relevance: 0.5 }])).toBe(
            '[Recalled memories]\n- (User, relevance=0.50): brent prefers execution over questions\n',
        );
    });

    it('adds the freshness note only when a memory is time-sensitive', () => {
        // A Project/Reference memory names something in a moving codebase, so the model
        // is told to verify it. A User/Feedback one describes the person and does not go
        // stale — emitting the note there would train the model to skip it.
        const block = renderRecallBlock([{ text: 'the retry lives in fetch.rs', memoryType: 'Project', relevance: 1 }]);
        expect(block).toBe(`${RECALL_HEADER}\n${RECALL_FRESHNESS_NOTE}\n- (Project, relevance=1.00): the retry lives in fetch.rs\n`);
        expect(renderRecallBlock([{ text: 'prefers dark mode', memoryType: 'User', relevance: 1 }])).not.toContain('Note:');
    });

    it('renders no block for an empty recall', () => {
        // A bare header would spend context telling the model it remembered nothing.
        expect(renderRecallBlock([])).toBeUndefined();
    });

    it('scores relevance as a fraction of the query tokens', () => {
        // Normalised to 0-1 so it is comparable between entries AND between languages —
        // a raw overlap count is neither, and it is rendered into the prompt.
        expect(relevanceScore('watchlist on marvin today', 'the watchlist lives on smoo-hub')).toBeCloseTo(0.5);
        expect(relevanceScore('', 'anything')).toBe(0);
    });

    it('does not let punctuation defeat a match', () => {
        // Scoring used to split on whitespace only, so "do you remember my name?" scored 0
        // against "the user's name is Dana" — the trailing '?' made `name?` fail — and the
        // memory was silently never recalled.
        expect(relevanceScore('do you remember my name?', "The user's name is Dana.")).toBeGreaterThan(0);
        expect(relevanceScore('watchlist!', 'the watchlist lives here')).toBeCloseTo(1);
    });

    it('populates relevance and type on recall', () => {
        const mem = new InMemoryMemory();
        mem.remember('the watchlist lives on smoo-hub', 'Project');
        const hits = mem.recall('watchlist on marvin today');
        expect(hits).toHaveLength(1);
        expect(hits[0].relevance).toBeCloseTo(0.5);
        expect(hits[0].memoryType).toBe('Project');
    });
});
