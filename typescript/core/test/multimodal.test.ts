import { describe, expect, it } from 'vitest';

import { applyCacheControl } from '../src/cacheControl.js';
import { userContent } from '../src/multimodal.js';

// Multimodal image attachments (pearl th-25ce5c), ported from the Rust reference.
// The load-bearing property is the NEGATIVE one: a turn without images must be
// byte-identical to before the field existed.

describe('userContent', () => {
    it('returns the plain string when there are no images', () => {
        expect(userContent('hello')).toBe('hello');
        expect(userContent('hello', [])).toBe('hello');
    });

    it('emits a text part then one image_url part per image, in order', () => {
        expect(
            userContent('what is this?', [{ url: 'data:image/png;base64,AAAA' }, { url: 'https://x/y.jpg', detail: 'high' }]),
        ).toEqual([
            { type: 'text', text: 'what is this?' },
            { type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } },
            { type: 'image_url', image_url: { url: 'https://x/y.jpg', detail: 'high' } },
        ]);
    });

    it('omits the text part when images are sent alone', () => {
        expect(userContent('', [{ url: 'data:image/png;base64,ZZZZ' }])).toEqual([
            { type: 'image_url', image_url: { url: 'data:image/png;base64,ZZZZ' } },
        ]);
    });

    it('omits detail entirely when unset', () => {
        const parts = userContent('hi', [{ url: 'https://x/y.jpg' }]) as Array<Record<string, unknown>>;
        expect(Object.keys(parts[1]!.image_url as object)).toEqual(['url']);
    });
});

describe('multimodal + prompt caching', () => {
    // The regression this pairing produces: cache_control marks the LAST message,
    // which in a vision turn IS the image-bearing one. Flattening it into a text
    // block drops the images silently.
    const visionBody = () => ({
        model: 'claude-sonnet-4-5',
        messages: [
            { role: 'system', content: 'be helpful' },
            { role: 'user', content: userContent('what is this?', [{ url: 'data:image/png;base64,AAAA' }]) },
        ],
    });

    it('a vision turn through a Claude-routing gateway still carries the image', () => {
        const body = visionBody();
        applyCacheControl(body);

        const content = body.messages[1]!.content as Array<Record<string, unknown>>;
        expect(Array.isArray(content)).toBe(true);
        expect(content.some((p) => p.type === 'image_url')).toBe(true);
        // Passed through untouched — no marker smuggled onto an image part.
        expect(content.every((p) => p.cache_control === undefined)).toBe(true);
    });

    it('still caches a text-only turn on the same route', () => {
        const body = {
            model: 'claude-sonnet-4-5',
            messages: [
                { role: 'system', content: 'be helpful' },
                { role: 'user', content: userContent('no images here') },
            ],
        };
        applyCacheControl(body);
        // Sanity: the guard is scoped to multimodal content, it didn't disable caching.
        expect(JSON.stringify(body)).toContain('cache_control');
    });
});
