/**
 * Prompt caching — the static/dynamic system-prompt split. The TypeScript port
 * of the Rust reference `smooth-operator-core::conversation::PromptCache`.
 *
 * A system prompt has two halves with very different churn rates: role
 * instructions and tool schemas barely change, while project context
 * (AGENTS.md / CLAUDE.md, the working set) changes every turn. Anthropic's
 * prompt cache keys on a PREFIX, so putting the volatile half first invalidates
 * the whole thing. {@link PROMPT_CACHE_BOUNDARY} splits them: everything above
 * the marker is static and hashed once for cache-key dedup, everything below is
 * dynamic and can be swapped without busting the static prefix.
 *
 * Feed the result to the agent as its instructions:
 *
 * ```ts
 * const cache = new PromptCache(`${rules}${PROMPT_CACHE_BOUNDARY}${projectContext}`);
 * new SmoothAgent(provider, { instructions: cache.fullPrompt() });
 * ```
 */

/**
 * Marker that splits a system prompt into a cacheable static portion and a
 * frequently-changing dynamic portion.
 */
export const PROMPT_CACHE_BOUNDARY = '__PROMPT_CACHE_BOUNDARY__';

/** A system prompt split at {@link PROMPT_CACHE_BOUNDARY}. */
export class PromptCache {
    /** The cacheable half (above the marker). */
    readonly staticPortion: string;
    /** The frequently-changing half (below the marker). */
    dynamicPortion: string;
    private readonly _staticHash: string;
    private readonly _staticTokens: number;

    /**
     * Split a system prompt at the boundary marker. With no marker the entire
     * prompt is treated as dynamic — nothing is claimed cacheable that the
     * caller didn't mark.
     */
    constructor(prompt: string) {
        const idx = prompt.indexOf(PROMPT_CACHE_BOUNDARY);
        if (idx < 0) {
            this.staticPortion = '';
            this.dynamicPortion = prompt;
        } else {
            this.staticPortion = prompt.slice(0, idx);
            this.dynamicPortion = prompt.slice(idx + PROMPT_CACHE_BOUNDARY.length);
        }
        this._staticHash = hashPromptPortion(this.staticPortion);
        this._staticTokens = this.staticPortion === '' ? 0 : Math.floor(this.staticPortion.length / 4) + 1;
    }

    /**
     * Reassemble static + boundary + dynamic. With no static portion the dynamic
     * half is returned alone, so a prompt that was never split round-trips
     * unchanged rather than gaining a stray marker.
     */
    fullPrompt(): string {
        if (this.staticPortion === '') return this.dynamicPortion;
        return `${this.staticPortion}${PROMPT_CACHE_BOUNDARY}${this.dynamicPortion}`;
    }

    /**
     * Swap the dynamic half, leaving the static half and its hash untouched —
     * the whole point of the split.
     */
    updateDynamic(dynamic: string): void {
        this.dynamicPortion = dynamic;
    }

    /**
     * Identifies the static portion for cache-key deduplication.
     *
     * Process-local only: it is compared against other hashes from THIS engine,
     * never sent on the wire, so it deliberately does not match the Rust
     * reference's value (Rust uses `DefaultHasher`, which is not reproducible
     * across languages — or even across Rust releases). The ported contract is
     * the behavior: same static text hashes the same, different static text
     * hashes differently, and `updateDynamic` never changes it.
     */
    staticHash(): string {
        return this._staticHash;
    }

    /** Estimated tokens the static portion saves on a cache hit. */
    cachedTokens(): number {
        return this._staticTokens;
    }
}

/**
 * FNV-1a (64-bit), the same non-cryptographic hash family the vector embedder
 * uses, rendered as 16 hex chars like the Rust reference. BigInt because a
 * 64-bit multiply overflows a JS number.
 */
function hashPromptPortion(s: string): string {
    const MASK = (1n << 64n) - 1n;
    let h = 14695981039346656037n;
    const bytes = new TextEncoder().encode(s);
    for (const b of bytes) {
        h = (h ^ BigInt(b)) & MASK;
        h = (h * 1099511628211n) & MASK;
    }
    return h.toString(16).padStart(16, '0');
}
