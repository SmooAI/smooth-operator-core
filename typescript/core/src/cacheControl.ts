/**
 * Anthropic prompt-cache markers on the outbound request. The TypeScript port of
 * the Rust reference's `supports_anthropic_cache_control` + `apply_cache_control`
 * (llm.rs), and the wire half of {@link PromptCache}.
 *
 * Kept as a standalone module rather than inline in `agent.ts` so the agent's
 * body-assembly path only needs a single call — the marking rules live here.
 */

/** The `{"type":"ephemeral"}` marker: Anthropic's default 5-minute TTL. */
const EPHEMERAL = { type: 'ephemeral' } as const;

/**
 * Does the configured upstream understand Anthropic-shaped `cache_control`?
 *
 * True when the model id looks Claude-ish, or is one of the known semantic
 * gateway aliases that route to Claude, AND the api base looks like a
 * LiteLLM-style gateway or `anthropic.*` directly.
 *
 * We deliberately do NOT send these to bare OpenAI / Gemini / Groq endpoints —
 * they 400 on unknown extension fields. A LiteLLM gateway's
 * `cache_control_injection_points` config is what actually forwards the markers
 * to Anthropic; without that gateway-side change this is a no-op.
 */
export function supportsAnthropicCacheControl(model: string | undefined, apiBaseUrl: string | undefined): boolean {
    if (!model || !apiBaseUrl) return false;
    const m = model.toLowerCase();
    const u = apiBaseUrl.toLowerCase();
    const looksClaude = m.includes('claude') || m.includes('sonnet') || m.includes('opus') || m.includes('haiku');
    // The generic `smooth-` prefix alone isn't enough — `smooth-fast` routes to a
    // Groq/Llama model, which would 400 on cache_control.
    const isClaudeAlias =
        m.startsWith('smooth-coding') || m.startsWith('smooth-thinking') || m.startsWith('smooth-planning') || m.startsWith('smooth-reviewing');
    const urlIsGateway = u.includes('litellm') || u.includes('gateway');
    const urlIsAnthropic = u.includes('anthropic.');
    return (looksClaude || isClaudeAlias) && (urlIsGateway || urlIsAnthropic);
}

/**
 * Attach `cache_control: ephemeral` to the strategic prefix boundaries, in place:
 *
 * 1. The last system message — caches the system prompt.
 * 2. The last tool definition — caches the tool block + the system prefix ahead of it.
 *    Highest-ROI breakpoint: the tool registry is large and near-constant within a run.
 * 3. The last message in history — caches the running conversation, so each turn
 *    inside the 5-minute window pays only for the new delta.
 *
 * Marking a block caches THAT block plus everything before it, so only the last
 * block of each prefix we want to reuse needs a marker.
 */
export function applyCacheControl(body: Record<string, unknown>): void {
    const messages = Array.isArray(body.messages) ? (body.messages as Array<Record<string, unknown>>) : [];
    const tools = Array.isArray(body.tools) ? (body.tools as Array<Record<string, unknown>>) : [];

    // 1. Last system message.
    for (let i = messages.length - 1; i >= 0; i--) {
        if (messages[i]!.role === 'system') {
            messages[i]!.content = wrapWithCacheControl(messages[i]!.content);
            break;
        }
    }

    // 2. Last tool — covers the whole tools array plus the system prefix.
    if (tools.length > 0) tools[tools.length - 1]!.cache_control = { ...EPHEMERAL };

    // 3. Last message, so turn-by-turn history caching extends. Skipped when the only
    //    message is the system we just marked (avoid double-marking it).
    if (messages.length > 1) {
        const last = messages[messages.length - 1]!;
        last.content = wrapWithCacheControl(last.content);
    }
}

/**
 * Rewrite string content into the single-text-block form carrying the marker.
 *
 * Empty/absent content (a tool-call-only assistant message) is returned untouched:
 * there is nothing to cache on it, and the marker on the last block before the
 * assistant turn already covers the prefix. Content already in array form — either
 * re-marked blocks or OpenAI multimodal parts — is handled without flattening: for
 * blocks the marker moves to the last one, and anything carrying a non-text part
 * (an image) is passed through unchanged, since flattening would silently drop the
 * image and prompt caching only applies to text prefixes anyway.
 */
function wrapWithCacheControl(content: unknown): unknown {
    if (typeof content === 'string') {
        if (content === '') return content;
        return [{ type: 'text', text: content, cache_control: { ...EPHEMERAL } }];
    }
    if (Array.isArray(content)) {
        const parts = content as Array<Record<string, unknown>>;
        if (parts.length === 0) return content;
        // Multimodal: leave images (and their sibling text parts) exactly as they are.
        if (parts.some((p) => p.type !== undefined && p.type !== 'text')) return content;
        // Drop any stale marker, then re-mark only the last block.
        const blocks = parts.map(({ cache_control: _stale, ...rest }) => rest as Record<string, unknown>);
        blocks[blocks.length - 1] = { ...blocks[blocks.length - 1]!, cache_control: { ...EPHEMERAL } };
        return blocks;
    }
    return content;
}
