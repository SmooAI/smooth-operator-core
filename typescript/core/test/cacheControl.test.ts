// Ports the Rust reference engine's cache_control gate + request-body tests (llm.rs).

import { describe, expect, it } from 'vitest';
import { applyCacheControl, supportsAnthropicCacheControl } from '../src/cacheControl.js';
import { MockLlmProvider } from '../src/llmProvider.js';
import { SmoothAgent } from '../src/agent.js';

describe('supportsAnthropicCacheControl', () => {
    it('recognizes Claude routes and rejects everything else', () => {
        // Claude model id + LiteLLM gateway url → cache it.
        expect(supportsAnthropicCacheControl('claude-sonnet-4-20250514', 'https://litellm.example.com/v1')).toBe(true);
        // Smooth-coding alias + gateway url → cache it.
        expect(supportsAnthropicCacheControl('smooth-coding-claude', 'https://gateway.example.com/v1')).toBe(true);
        // Direct Anthropic API + Claude id → cache it.
        expect(supportsAnthropicCacheControl('claude-opus-4', 'https://api.anthropic.com/v1')).toBe(true);
        // GPT model on OpenAI → no cache control (would 400).
        expect(supportsAnthropicCacheControl('gpt-4o', 'https://api.openai.com/v1')).toBe(false);
        // Gemini-compat → no cache control.
        expect(supportsAnthropicCacheControl('gemini-1.5-pro', 'https://generativelanguage.googleapis.com')).toBe(false);
        // Claude id but bare OpenAI url (mis-configured) — still gated off.
        expect(supportsAnthropicCacheControl('claude-3-sonnet', 'https://api.openai.com/v1')).toBe(false);
        // smooth-fast routes to Groq/Llama via the gateway — must NOT be cached.
        expect(supportsAnthropicCacheControl('smooth-fast', 'https://gateway.example.com/v1')).toBe(false);
    });

    it('is off when the client reports no base url', () => {
        // The mock client sets no apiBaseUrl, so nothing is ever marked under test.
        expect(supportsAnthropicCacheControl('claude-opus-4', undefined)).toBe(false);
    });
});

describe('applyCacheControl', () => {
    it('marks the system message, the LAST tool, and the last message', () => {
        const body: Record<string, unknown> = {
            model: 'smooth-coding-claude',
            messages: [
                { role: 'system', content: 'You are smooth.' },
                { role: 'user', content: 'Hi' },
            ],
            tools: [
                { type: 'function', function: { name: 'bash', description: 'Run a command', parameters: {} } },
                { type: 'function', function: { name: 'file_write', description: 'Write a file', parameters: {} } },
            ],
        };

        applyCacheControl(body);
        const messages = body.messages as Array<Record<string, unknown>>;
        const tools = body.tools as Array<Record<string, unknown>>;

        const sysContent = messages[0]!.content as Array<Record<string, unknown>>;
        expect(Array.isArray(sysContent)).toBe(true);
        expect(sysContent[0]).toEqual({ type: 'text', text: 'You are smooth.', cache_control: { type: 'ephemeral' } });

        expect(tools[0]!.cache_control).toBeUndefined();
        expect(tools[1]!.cache_control).toEqual({ type: 'ephemeral' });

        const lastContent = messages[1]!.content as Array<Record<string, unknown>>;
        expect(Array.isArray(lastContent)).toBe(true);
        expect(lastContent[0]!.cache_control).toEqual({ type: 'ephemeral' });
    });

    it('leaves empty content alone', () => {
        // A tool-call-only assistant message has no prose to cache.
        const body: Record<string, unknown> = {
            messages: [
                { role: 'system', content: 'sys' },
                { role: 'assistant', content: '' },
            ],
            tools: [],
        };
        applyCacheControl(body);
        expect((body.messages as Array<Record<string, unknown>>)[1]!.content).toBe('');
    });

    it('passes multimodal content through without flattening it', () => {
        // Flattening would silently drop the image; caching only covers text prefixes.
        const parts = [
            { type: 'text', text: 'look' },
            { type: 'image_url', image_url: { url: 'data:image/png;base64,ZZZZ' } },
        ];
        const body: Record<string, unknown> = {
            messages: [
                { role: 'system', content: 'sys' },
                { role: 'user', content: parts },
            ],
            tools: [],
        };
        applyCacheControl(body);
        expect((body.messages as Array<Record<string, unknown>>)[1]!.content).toBe(parts);
    });

    it('re-marks only the last block when content is already in block form', () => {
        const body: Record<string, unknown> = {
            messages: [
                { role: 'system', content: 'sys' },
                {
                    role: 'user',
                    content: [
                        { type: 'text', text: 'first', cache_control: { type: 'ephemeral' } },
                        { type: 'text', text: 'second' },
                    ],
                },
            ],
            tools: [],
        };
        applyCacheControl(body);
        const blocks = (body.messages as Array<Record<string, unknown>>)[1]!.content as Array<Record<string, unknown>>;
        expect(blocks[0]!.cache_control).toBeUndefined();
        expect(blocks[1]!.cache_control).toEqual({ type: 'ephemeral' });
    });
});

describe('SmoothAgent prompt-cache wiring', () => {
    it('sends a byte-identical body when the client reports no base url', async () => {
        const provider = new MockLlmProvider().pushText('done');
        const agent = new SmoothAgent(provider, { instructions: 'You are smooth.', model: 'claude-opus-4' });

        await agent.run('Hi');

        const sent = provider.calls[0]!.body as Record<string, unknown>;
        expect(JSON.stringify(sent)).not.toContain('cache_control');
        const messages = sent.messages as Array<Record<string, unknown>>;
        expect(messages[0]!.content).toBe('You are smooth.');
    });

    it('marks the body when the client reports a Claude-routing gateway', async () => {
        const provider = new MockLlmProvider().pushText('done');
        // The seam the real gateway client populates from the OpenAI SDK.
        (provider as unknown as { apiBaseUrl?: string }).apiBaseUrl = 'https://gateway.example.com/v1';
        const agent = new SmoothAgent(provider, { instructions: 'You are smooth.', model: 'smooth-coding-claude' });

        await agent.run('Hi');

        const sent = provider.calls[0]!.body as Record<string, unknown>;
        const messages = sent.messages as Array<Record<string, unknown>>;
        expect(Array.isArray(messages[0]!.content)).toBe(true);
        expect(JSON.stringify(sent)).toContain('ephemeral');
    });
});
