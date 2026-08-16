/**
 * The gateway reports per-request cost ONLY in a response header.
 *
 * The TS engine takes an injected OpenAI-compatible client, so it has no HTTP
 * client of its own to read headers from (unlike the Go engine). These cover the
 * parser and the seam the cost flows through, so a client that surfaces headers —
 * `openai`'s `.withResponse()`, or a wrapper that pre-parses — lands a real
 * `costUsd` on the turn. A real HTTP round-trip is exercised too, via a local
 * server, to prove the parser works against actual `Headers`.
 */
import { createServer, type Server } from 'node:http';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { AgentOptions, SmoothAgent } from '../src/agent.js';
import { CostTracker, parseGatewayCost } from '../src/cost.js';

describe('parseGatewayCost — precedence', () => {
    it('prefers margin, then original, then the legacy header', () => {
        expect(
            parseGatewayCost({
                'x-litellm-response-cost-margin-amount': '3.0e-05',
                'x-litellm-response-cost-original': '1.0e-05',
                'x-litellm-response-cost': '9.0e-05',
            }),
        ).toBe(3.0e-5);
        expect(
            parseGatewayCost({
                'x-litellm-response-cost-original': '1.0e-05',
                'x-litellm-response-cost': '9.0e-05',
            }),
        ).toBe(1.0e-5);
        expect(parseGatewayCost({ 'x-litellm-response-cost': '1.47e-05' })).toBe(1.47e-5);
    });

    it('reads the generic gateway fallbacks', () => {
        expect(parseGatewayCost({ 'x-response-cost': '0.5' })).toBe(0.5);
        expect(parseGatewayCost({ 'x-cost-usd': '0.25' })).toBe(0.25);
    });

    // The distinction the whole fix rests on: absent and zero are BOTH
    // "unmeasured", never a recorded $0.
    it('returns undefined for absent, zero, negative and unparseable', () => {
        expect(parseGatewayCost({})).toBeUndefined();
        expect(parseGatewayCost(undefined)).toBeUndefined();
        expect(parseGatewayCost({ 'x-litellm-response-cost': '0' })).toBeUndefined();
        expect(parseGatewayCost({ 'x-litellm-response-cost': '-1' })).toBeUndefined();
        expect(parseGatewayCost({ 'x-litellm-response-cost': 'not-a-number' })).toBeUndefined();
    });

    it('falls through a zero margin to a real original', () => {
        expect(
            parseGatewayCost({
                'x-litellm-response-cost-margin-amount': '0',
                'x-litellm-response-cost-original': '2.5e-05',
            }),
        ).toBe(2.5e-5);
    });
});

describe('parseGatewayCost — against real HTTP headers', () => {
    let server: Server;
    let url = '';

    beforeAll(async () => {
        server = createServer((_req, res) => {
            res.setHeader('x-litellm-response-cost', '1.47e-05');
            res.end('{}');
        });
        await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
        const addr = server.address();
        url = `http://127.0.0.1:${typeof addr === 'object' && addr ? addr.port : 0}/`;
    });
    afterAll(() => new Promise<void>((resolve) => server.close(() => resolve())));

    it('reads a fetch Headers object case-insensitively', async () => {
        const res = await fetch(url);
        await res.text();
        expect(parseGatewayCost(res.headers)).toBe(1.47e-5);
    });
});

describe('CostTracker.recordWithGatewayCost', () => {
    const pricing = { m: { inputPerMTok: 1000, outputPerMTok: 1000 } };
    const usage = { promptTokens: 10, completionTokens: 5 };

    it('prefers the gateway cost over the local estimate', () => {
        const tracker = new CostTracker();
        tracker.recordWithGatewayCost('m', usage, 0.25, pricing);
        expect(tracker.costUsd).toBe(0.25);
        expect(tracker.usage.promptTokens).toBe(10);
    });

    it('falls back to local pricing when unmeasured — never records 0', () => {
        const tracker = new CostTracker();
        tracker.recordWithGatewayCost('m', usage, undefined, pricing);
        expect(tracker.costUsd).toBeGreaterThan(0);
    });
});

/** A client whose response carries whatever cost seam the test is exercising. */
function clientReturning(extra: Record<string, unknown>) {
    return {
        chat: {
            completions: {
                create: async () => ({
                    choices: [{ message: { content: 'hi', tool_calls: null } }],
                    usage: { prompt_tokens: 10, completion_tokens: 5 },
                    ...extra,
                }),
            },
        },
    } as unknown as ConstructorParameters<typeof SmoothAgent>[0];
}

describe('the turn folds the gateway cost into costUsd', () => {
    const options: AgentOptions = { model: 'm', pricing: { m: { inputPerMTok: 1000, outputPerMTok: 1000 } } };

    it('honors a cost a wrapping client already parsed', async () => {
        const agent = new SmoothAgent(clientReturning({ gatewayCostUsd: 0.25 }), options);
        const result = await agent.run('hi');
        expect(result.costUsd).toBe(0.25);
    });

    it('parses raw headers hung off the response (the .withResponse() shape)', async () => {
        const agent = new SmoothAgent(clientReturning({ headers: { 'x-litellm-response-cost': '0.75' } }), options);
        const result = await agent.run('hi');
        expect(result.costUsd).toBe(0.75);
    });

    it('falls back to local pricing when the gateway measured nothing', async () => {
        const agent = new SmoothAgent(clientReturning({}), options);
        const result = await agent.run('hi');
        // 15 tokens at $1000/1M = $0.015 — an estimate, but not a bogus $0.
        expect(result.costUsd).toBeGreaterThan(0);
        expect(result.costUsd).not.toBe(0.25);
    });
});
