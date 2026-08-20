/**
 * Unit tests for the LlmProvider seam and the reusable MockLlmProvider.
 *
 * Proves the mock replays scripted text + tool-call + error responses in FIFO
 * order, and records the requests (messages + tool specs) the agent sent — the
 * behavioral parity surface of the Rust reference's `MockLlmClient`.
 */

import { describe, expect, it } from 'vitest';
import { AgentOptions, SmoothAgent, Tool } from '../src/agent.js';
import { MockLlmProvider, SCRIPTED_USAGE } from '../src/llmProvider.js';

describe('MockLlmProvider', () => {
    it('replays text responses in FIFO order', async () => {
        const mock = new MockLlmProvider();
        mock.pushText('first').pushText('second');

        const r1 = await mock.chat.completions.create({ messages: [] });
        const r2 = await mock.chat.completions.create({ messages: [] });

        expect(r1.choices[0].message.content).toBe('first');
        expect(r2.choices[0].message.content).toBe('second');
    });

    // The cross-language invariant (pearl th-4f1263): a scripted text or tool-call
    // response reports 10 prompt / 5 completion tokens in EVERY engine's mock, and a
    // drained script still reports nothing. Change these numbers here and the shared
    // server scenario corpus goes red.
    it('reports the shared scripted-usage convention', async () => {
        const mock = new MockLlmProvider();
        mock.pushText('hi').pushToolCall('call_1', 'search', '{}');

        for (let i = 0; i < 2; i++) {
            const response = await mock.chat.completions.create({ messages: [] });
            expect(response.usage).toEqual(SCRIPTED_USAGE);
        }

        // Drained script: "the script ran out" must stay distinguishable from "the
        // model answered", so the fallback reports nothing.
        const drained = await mock.chat.completions.create({ messages: [] });
        expect(drained.usage).toBeNull();
    });

    it('records messages and tools', async () => {
        const mock = new MockLlmProvider();
        mock.pushText('ok');
        const tools = [{ type: 'function', function: { name: 'search', description: 'search', parameters: {} } }];

        await mock.chat.completions.create({
            messages: [
                { role: 'system', content: 'be helpful' },
                { role: 'user', content: 'hello' },
            ],
            tools,
        });

        expect(mock.callCount).toBe(1);
        const call = mock.lastCall!;
        expect(call.messages).toHaveLength(2);
        expect(call.messages[0].content).toBe('be helpful');
        expect(call.messages[1].content).toBe('hello');
        expect((call.tools![0] as Record<string, Record<string, unknown>>).function.name).toBe('search');
    });

    it('returns a benign terminal response when the script is empty', async () => {
        const mock = new MockLlmProvider();
        const resp = await mock.chat.completions.create({ messages: [] });
        expect(resp.choices[0].message.content).toBe('');
        expect(resp.choices[0].message.tool_calls).toBeUndefined();
    });

    it('scripts errors', async () => {
        const mock = new MockLlmProvider();
        mock.pushError('rate limited');
        await expect(mock.chat.completions.create({ messages: [] })).rejects.toThrow('rate limited');
    });

    it('carries a scripted tool call', async () => {
        const mock = new MockLlmProvider();
        mock.pushToolCall('call_1', 'get_weather', '{"city": "SF"}');
        const resp = await mock.chat.completions.create({ messages: [] });
        const message = resp.choices[0].message;
        expect(message.tool_calls![0].function.name).toBe('get_weather');
        expect(message.tool_calls![0].function.arguments).toBe('{"city": "SF"}');
    });

    it('can be constructed from a pre-assembled script', async () => {
        const mock = new MockLlmProvider([{ content: 'scripted' }]);
        const resp = await mock.chat.completions.create({ messages: [] });
        expect(resp.choices[0].message.content).toBe('scripted');
    });

    it('drives a full agent turn and records the request', async () => {
        const echo: Tool = {
            name: 'echo',
            description: 'Echoes input back',
            parameters: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] },
            execute: async (args) => String(args.text ?? ''),
        };
        const mock = new MockLlmProvider();
        mock.pushToolCall('call-1', 'echo', '{"text": "hello tools"}').pushText('done');

        const agent = new SmoothAgent(mock, { tools: [echo] } satisfies AgentOptions);
        const result = await agent.run('use echo');

        expect(result.text).toBe('done');
        expect(result.toolCalls).toBe(1);
        // Two model calls were recorded; the second saw the tool result fed back.
        expect(mock.callCount).toBe(2);
        const secondCallMessages = mock.calls[1].messages;
        expect(secondCallMessages.some((m) => m.role === 'tool' && m.content === 'hello tools')).toBe(true);
        // The tool spec was advertised on every call.
        expect((mock.calls[0].tools![0] as Record<string, Record<string, unknown>>).function.name).toBe('echo');
    });

    // A multibyte character landing on a chunk boundary must not be split.
    //
    // Regression for th-6fdd1c. The sibling Go mock sliced by BYTES and mangled an
    // em-dash; this mock slices a JS string, which is UTF-16 — safe for the BMP
    // (em-dash, accents, CJK) but NOT for astral characters, where `.slice()` can cut
    // an emoji between its surrogate pair.
    //
    // Each chunk is checked INDIVIDUALLY, and by round-tripping through UTF-8 rather
    // than by regex: concatenating the pieces heals a lone surrogate in memory, but
    // encoding one chunk on its own — which is what the wire does — turns it into
    // U+FFFD irreversibly.
    it.each([
        ['em-dash', 'Pro it is \u2014 pulling that quote up.'],
        ['emoji (surrogate pair)', 'ok \u{1F642} done'],
        ['accents + CJK', 'caf\u00e9 m\u00fcnchen \u6771\u4eac'],
    ])('streams %s without splitting a code point', async (_label, text) => {
        const mock = new MockLlmProvider();
        mock.pushText(text);

        let reassembled = '';
        for await (const chunk of mock.chat.completions.createStream({ messages: [] })) {
            const piece = chunk.choices[0]?.delta?.content;
            if (!piece) continue;
            expect(Buffer.from(piece, 'utf8').toString('utf8')).toBe(piece);
            reassembled += piece;
        }
        expect(reassembled).toBe(text);
    });

    // The same hazard on tool-call arguments, which are JSON and can carry non-ASCII.
    it('streams multibyte tool-call arguments without splitting a code point', async () => {
        const args = JSON.stringify({ city: 'M\u00fcnchen', reaction: '\u{1F642}' });
        const mock = new MockLlmProvider();
        mock.pushToolCall('call-1', 'lookup', args);

        let reassembled = '';
        for await (const chunk of mock.chat.completions.createStream({ messages: [] })) {
            const piece = chunk.choices[0]?.delta?.tool_calls?.[0]?.function?.arguments;
            if (!piece) continue;
            expect(Buffer.from(piece, 'utf8').toString('utf8')).toBe(piece);
            reassembled += piece;
        }
        expect(reassembled).toBe(args);
    });
});
