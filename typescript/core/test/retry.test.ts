/**
 * Retry-with-exponential-backoff around the model call. Driven by the reusable
 * `MockLlmProvider` (it can script transient errors via `pushError`). Backoff is set
 * to 0 so no real time is spent sleeping.
 */

import { describe, expect, it } from 'vitest';
import { AgentOptions, SmoothAgent } from '../src/agent.js';
import { MockLlmProvider } from '../src/llmProvider.js';

function makeAgent(client: MockLlmProvider, options: AgentOptions = {}): SmoothAgent {
    return new SmoothAgent(client, options);
}

describe('SmoothAgent retry', () => {
    it('retries then succeeds', async () => {
        // Errors k times then a text reply; maxRetries >= k → the turn succeeds and the
        // model is called exactly k+1 times.
        const client = new MockLlmProvider();
        client.pushError('rate limited').pushError('rate limited').pushText('ok');
        const agent = makeAgent(client, { maxRetries: 2, retryBackoffMs: 0 });
        const result = await agent.run('hi');
        expect(result.text).toBe('ok');
        expect(client.callCount).toBe(3); // k+1 = 2 failures + 1 success
    });

    it('propagates the error when retries are exhausted', async () => {
        // Errors maxRetries+1 times → the provider error propagates (the turn fails).
        const client = new MockLlmProvider();
        client.pushError('boom').pushError('boom');
        const agent = makeAgent(client, { maxRetries: 1, retryBackoffMs: 0 });
        await expect(agent.run('hi')).rejects.toThrow('boom');
        expect(client.callCount).toBe(2); // maxRetries + 1 attempts
    });

    it('does not retry by default (maxRetries=0)', async () => {
        // Default maxRetries=0 → a single error propagates immediately (one attempt).
        const client = new MockLlmProvider();
        client.pushError('nope').pushText('never reached');
        const agent = makeAgent(client, {});
        await expect(agent.run('hi')).rejects.toThrow('nope');
        expect(client.callCount).toBe(1);
    });
});
