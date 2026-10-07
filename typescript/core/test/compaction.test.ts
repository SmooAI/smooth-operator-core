import { describe, expect, it } from 'vitest';
import { compact, estimateTokens } from '../src/compaction.js';

type Message = Record<string, unknown>;
const msg = (role: string, content: string): Message => ({ role, content });

describe('compaction', () => {
    it('leaves a conversation under budget unchanged', () => {
        const msgs = [msg('system', 'sys'), msg('user', 'hi'), msg('assistant', 'hello')];
        expect(compact(msgs, 8000)).toEqual(msgs);
    });

    it('is disabled when budget is non-positive', () => {
        const msgs = [msg('user', 'x'.repeat(10_000))];
        expect(compact(msgs, 0)).toEqual(msgs);
    });

    it('drops oldest, keeps system + recent, fits budget', () => {
        const big = 'word '.repeat(200);
        const msgs = [
            msg('system', 'you are helpful'),
            msg('user', `OLDEST ${big}`),
            msg('assistant', `old reply ${big}`),
            msg('user', `MIDDLE ${big}`),
            msg('assistant', `mid reply ${big}`),
            msg('user', 'NEWEST question'),
        ];
        const out = compact(msgs, 400);
        expect(out[0].role).toBe('system');
        const contents = out.map((m) => m.content as string).join(' ');
        expect(contents).toContain('NEWEST question');
        expect(contents).not.toContain('OLDEST');
        expect(out.reduce((s, m) => s + estimateTokens(m), 0)).toBeLessThanOrEqual(400);
    });

    it('never starts the kept window on an orphan tool message', () => {
        const big = 'token '.repeat(300);
        const msgs: Message[] = [
            msg('system', 'sys'),
            msg('user', `q ${big}`),
            { role: 'assistant', content: '', tool_calls: [{ id: 'c1', function: { name: 't', arguments: '{}' } }] },
            { role: 'tool', tool_call_id: 'c1', content: `result ${big}` },
            msg('assistant', 'final answer'),
        ];
        const out = compact(msgs, 200);
        const nonSystem = out.filter((m) => m.role !== 'system');
        expect(nonSystem.length).toBeGreaterThan(0);
        expect(nonSystem[0].role).not.toBe('tool');
    });

    it('drops every result of a parallel tool group whose call was trimmed (SMOODEV-3704)', () => {
        const big = 'token '.repeat(300);
        const msgs: Message[] = [
            msg('system', 'sys'),
            msg('user', 'q'),
            {
                role: 'assistant',
                content: '',
                tool_calls: ['p1', 'p2', 'p3'].map((id) => ({ id, function: { name: 't', arguments: '{}' } })),
            },
            { role: 'tool', tool_call_id: 'p1', content: `result ${big}` },
            { role: 'tool', tool_call_id: 'p2', content: 'two' },
            { role: 'tool', tool_call_id: 'p3', content: 'three' },
            msg('assistant', 'final answer'),
        ];
        // The budget fits p2, p3 and the reply but not p1, so the cut lands inside the group.
        const out = compact(msgs, 50);
        const announced = new Set(out.flatMap((m) => ((m.tool_calls as Array<{ id: string }> | undefined) ?? []).map((tc) => tc.id)));
        for (const m of out.filter((m) => m.role === 'tool')) {
            expect(announced.has(m.tool_call_id as string)).toBe(true);
        }
        expect(out.at(-1)?.content).toBe('final answer');
    });
});
