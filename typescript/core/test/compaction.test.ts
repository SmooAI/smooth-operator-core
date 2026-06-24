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
});
