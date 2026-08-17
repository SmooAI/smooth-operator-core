/**
 * The real HTTP client, against a real local gateway.
 *
 * Every assertion here is a round-trip: a `node:http` server speaking the
 * OpenAI `/chat/completions` shape (JSON and SSE), driven through
 * {@link createGatewayClient} and a live {@link SmoothAgent}. Nothing is mocked
 * below the socket, so these cover the things a mock cannot: that the SSE framing
 * parses, that `metadata` reaches the wire (and is ABSENT when unset), and above
 * all that the cost header is read on the streaming path. The response headers
 * survive the body being consumed just fine; the regression core#102 fixed in Rust
 * was keeping only the stream and dropping the response, leaving nothing to read a
 * header off at all.
 */
import { createServer, type IncomingMessage, type Server } from 'node:http';
import { afterEach, describe, expect, it } from 'vitest';

import { SmoothAgent, type AgentOptions } from '../src/agent.js';
import { createGatewayClient } from '../src/gatewayClient.js';

/** What the gateway should answer with for one test. */
interface Scripted {
    /** Response headers (the cost header lives here, and nowhere else). */
    headers?: Record<string, string>;
    /** Assistant text for the non-streaming reply. */
    text?: string;
    /** Token usage reported back. */
    usage?: { prompt_tokens: number; completion_tokens: number };
    /** SSE content deltas for the streaming reply. */
    deltas?: string[];
}

/** Every request body the server received, in order — for on-the-wire assertions. */
let received: Array<Record<string, unknown>> = [];
let server: Server | undefined;

async function startGateway(script: Scripted): Promise<string> {
    received = [];
    server = createServer((req: IncomingMessage, res) => {
        const chunks: Buffer[] = [];
        req.on('data', (c: Buffer) => chunks.push(c));
        req.on('end', () => {
            const body = JSON.parse(Buffer.concat(chunks).toString()) as Record<string, unknown>;
            received.push(body);
            for (const [name, value] of Object.entries(script.headers ?? {})) res.setHeader(name, value);

            if (body.stream === true) {
                res.setHeader('Content-Type', 'text/event-stream');
                for (const delta of script.deltas ?? []) {
                    res.write(`data: ${JSON.stringify({ choices: [{ index: 0, delta: { content: delta } }] })}\n\n`);
                }
                if (script.usage) {
                    res.write(`data: ${JSON.stringify({ choices: [], usage: script.usage })}\n\n`);
                }
                res.write('data: [DONE]\n\n');
                res.end();
                return;
            }

            res.setHeader('Content-Type', 'application/json');
            res.end(
                JSON.stringify({
                    model: 'm',
                    choices: [{ index: 0, message: { role: 'assistant', content: script.text ?? '' } }],
                    usage: script.usage,
                }),
            );
        });
    });
    await new Promise<void>((resolve) => server!.listen(0, '127.0.0.1', resolve));
    const addr = server.address();
    return `http://127.0.0.1:${typeof addr === 'object' && addr ? addr.port : 0}/v1`;
}

afterEach(async () => {
    if (server) await new Promise<void>((resolve) => server!.close(() => resolve()));
    server = undefined;
});

// $1000/1M tokens makes the local estimate large and obviously distinct from any
// gateway-reported cost, so "which number won" is never ambiguous.
const PRICING = { m: { inputPerMTok: 1000, outputPerMTok: 1000 } };
const options: AgentOptions = { model: 'm', pricing: PRICING };

describe('createGatewayClient — non-streaming', () => {
    it('round-trips content and usage through a live turn', async () => {
        const baseURL = await startGateway({ text: 'hello from the gateway', usage: { prompt_tokens: 11, completion_tokens: 7 } });
        const result = await new SmoothAgent(createGatewayClient({ baseURL, apiKey: 'k' }), options).run('hi');

        expect(result.text).toBe('hello from the gateway');
        expect(result.usage).toEqual({ promptTokens: 11, completionTokens: 7 });
        expect(received[0].model).toBe('m');
        expect(received[0].messages).toEqual([{ role: 'user', content: 'hi' }]);
    });

    it('folds the gateway cost header into the turn cost', async () => {
        const baseURL = await startGateway({
            text: 'hi',
            usage: { prompt_tokens: 10, completion_tokens: 5 },
            headers: { 'x-litellm-response-cost-margin-amount': '0.25' },
        });
        const result = await new SmoothAgent(createGatewayClient({ baseURL, apiKey: 'k' }), options).run('hi');

        // The gateway's number wins outright — not summed with, not shadowed by, the
        // $0.015 local estimate for these 15 tokens.
        expect(result.costUsd).toBe(0.25);
    });

    it('falls back to local pricing when the gateway reports no cost', async () => {
        const baseURL = await startGateway({ text: 'hi', usage: { prompt_tokens: 10, completion_tokens: 5 } });
        const result = await new SmoothAgent(createGatewayClient({ baseURL, apiKey: 'k' }), options).run('hi');

        // Unmeasured must stay unmeasured: an estimate, never a recorded $0.
        expect(result.costUsd).toBeCloseTo(0.015, 10);
    });

    it('takes the first NON-ZERO header, so a zero margin does not zero real spend', async () => {
        const baseURL = await startGateway({
            text: 'hi',
            usage: { prompt_tokens: 10, completion_tokens: 5 },
            headers: { 'x-litellm-response-cost-margin-amount': '0', 'x-litellm-response-cost-original': '0.5' },
        });
        const result = await new SmoothAgent(createGatewayClient({ baseURL, apiKey: 'k' }), options).run('hi');

        expect(result.costUsd).toBe(0.5);
    });
});

describe('createGatewayClient — streaming', () => {
    it('yields SSE content deltas and assembles the final text', async () => {
        const baseURL = await startGateway({ deltas: ['Hel', 'lo ', 'world'], usage: { prompt_tokens: 4, completion_tokens: 3 } });
        const agent = new SmoothAgent(createGatewayClient({ baseURL, apiKey: 'k' }), options);

        const texts: string[] = [];
        let final;
        for await (const event of agent.runStream('hi')) {
            if (event.type === 'text') texts.push(event.text);
            if (event.type === 'done') final = event.response;
        }

        expect(texts).toEqual(['Hel', 'lo ', 'world']);
        expect(final?.text).toBe('Hello world');
        expect(final?.usage).toEqual({ promptTokens: 4, completionTokens: 3 });
        expect(received[0].stream).toBe(true);
    });

    it('reads the cost header BEFORE the SSE body and folds it into the turn', async () => {
        const baseURL = await startGateway({
            deltas: ['hi'],
            usage: { prompt_tokens: 10, completion_tokens: 5 },
            headers: { 'x-litellm-response-cost': '0.75' },
        });
        const agent = new SmoothAgent(createGatewayClient({ baseURL, apiKey: 'k' }), options);

        let final;
        for await (const event of agent.runStream('hi')) {
            if (event.type === 'done') final = event.response;
        }

        // The whole point of the streaming path: a plain `create({stream:true})` returns
        // only the stream, leaving no response to read a header off at all — so a $0.75
        // here proves the client kept the response, not that it read it early.
        expect(final?.costUsd).toBe(0.75);
    });

    it('falls back to local pricing when a stream reports no cost', async () => {
        const baseURL = await startGateway({ deltas: ['hi'], usage: { prompt_tokens: 10, completion_tokens: 5 } });
        const agent = new SmoothAgent(createGatewayClient({ baseURL, apiKey: 'k' }), options);

        let final;
        for await (const event of agent.runStream('hi')) {
            if (event.type === 'done') final = event.response;
        }

        expect(final?.costUsd).toBeCloseTo(0.015, 10);
    });
});

describe('createGatewayClient — request metadata (core#100)', () => {
    it('sends metadata on the wire when set', async () => {
        const baseURL = await startGateway({ text: 'hi' });
        const agent = new SmoothAgent(createGatewayClient({ baseURL, apiKey: 'k' }), { ...options, metadata: { agent_slug: 'support' } });
        await agent.run('hi');

        expect(received[0].metadata).toEqual({ agent_slug: 'support' });
    });

    it('sends metadata on the streaming wire too', async () => {
        const baseURL = await startGateway({ deltas: ['hi'] });
        const agent = new SmoothAgent(createGatewayClient({ baseURL, apiKey: 'k' }), { ...options, metadata: { agent_slug: 'support' } });
        for await (const _event of agent.runStream('hi')) {
            // drain
        }

        expect(received[0].metadata).toEqual({ agent_slug: 'support' });
    });

    it('omits the field entirely when unset — byte-identical to a client without it', async () => {
        const baseURL = await startGateway({ text: 'hi' });
        await new SmoothAgent(createGatewayClient({ baseURL, apiKey: 'k' }), options).run('hi');

        expect('metadata' in received[0]).toBe(false);
    });
});
