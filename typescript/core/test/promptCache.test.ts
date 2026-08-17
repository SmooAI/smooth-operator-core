// Ports the Rust reference engine's PromptCache tests (conversation.rs).

import { describe, expect, it } from 'vitest';
import { PROMPT_CACHE_BOUNDARY, PromptCache } from '../src/promptCache.js';

describe('PromptCache', () => {
    it('splits at the boundary', () => {
        const c = new PromptCache(`static rules here${PROMPT_CACHE_BOUNDARY}dynamic context here`);
        expect(c.staticPortion).toBe('static rules here');
        expect(c.dynamicPortion).toBe('dynamic context here');
    });

    it('treats everything as dynamic when there is no marker', () => {
        const prompt = 'no marker in this prompt';
        const c = new PromptCache(prompt);
        expect(c.staticPortion).toBe('');
        expect(c.dynamicPortion).toBe(prompt);
    });

    it('combines static + boundary + dynamic in fullPrompt', () => {
        const prompt = `You are an assistant.${PROMPT_CACHE_BOUNDARY}Project: Smooth`;
        expect(new PromptCache(prompt).fullPrompt()).toBe(prompt);
    });

    it('round-trips an unsplit prompt without adding a marker', () => {
        expect(new PromptCache('all dynamic').fullPrompt()).toBe('all dynamic');
    });

    it('updateDynamic only changes the dynamic portion', () => {
        const c = new PromptCache(`static${PROMPT_CACHE_BOUNDARY}old dynamic`);
        const original = c.staticHash();

        c.updateDynamic('new dynamic');

        expect(c.dynamicPortion).toBe('new dynamic');
        expect(c.staticPortion).toBe('static');
        expect(c.staticHash()).toBe(original);
    });

    it('hashes the static portion deterministically', () => {
        const prompt = `same static${PROMPT_CACHE_BOUNDARY}dynamic`;
        expect(new PromptCache(prompt).staticHash()).toBe(new PromptCache(prompt).staticHash());
    });

    it('changes the hash when the static portion changes', () => {
        const a = new PromptCache(`static A${PROMPT_CACHE_BOUNDARY}dynamic`);
        const b = new PromptCache(`static B${PROMPT_CACHE_BOUNDARY}dynamic`);
        expect(a.staticHash()).not.toBe(b.staticHash());
    });

    it('estimates cached tokens from the static portion', () => {
        // "static text" is 11 chars => 11/4 + 1 = 3
        expect(new PromptCache(`static text${PROMPT_CACHE_BOUNDARY}dynamic`).cachedTokens()).toBe(Math.floor(11 / 4) + 1);
        // No marker => empty static => 0 tokens.
        expect(new PromptCache('all dynamic').cachedTokens()).toBe(0);
    });
});
