/**
 * Token-aware conversation compaction (sliding window).
 *
 * Phase-1 sibling of the reference engines' compaction. When a conversation's
 * estimated token count exceeds a budget, drop the oldest non-system messages
 * (keeping the system prompt and most recent turns) so the next model call stays
 * within context. A coarse char/4 token estimate is used.
 *
 * Safety: the kept window never *starts* on a `tool` message (which would orphan
 * a tool result whose preceding `assistant` tool_call was trimmed).
 */

const CHARS_PER_TOKEN = 4;

type Message = Record<string, unknown>;

export function estimateTokens(message: Message): number {
    let text = typeof message.content === 'string' ? message.content : '';
    const toolCalls = (message.tool_calls as Array<{ function?: { name?: string; arguments?: string } }> | undefined) ?? [];
    for (const tc of toolCalls) {
        text += String(tc.function?.name ?? '') + String(tc.function?.arguments ?? '');
    }
    return Math.max(1, Math.ceil(text.length / CHARS_PER_TOKEN));
}

/**
 * Return `messages` trimmed to roughly `maxTokens`, preserving system messages
 * and the most recent turns. Returns the input unchanged when already within
 * budget or when `maxTokens` is non-positive (disabled).
 */
export function compact(messages: Message[], maxTokens: number): Message[] {
    if (maxTokens <= 0) return messages;

    const system = messages.filter((m) => m.role === 'system');
    const rest = messages.filter((m) => m.role !== 'system');

    const systemTokens = system.reduce((sum, m) => sum + estimateTokens(m), 0);
    const total = systemTokens + rest.reduce((sum, m) => sum + estimateTokens(m), 0);
    if (total <= maxTokens) return messages;

    const budget = maxTokens - systemTokens;
    const kept: Message[] = [];
    let running = 0;
    for (let i = rest.length - 1; i >= 0; i--) {
        const t = estimateTokens(rest[i]);
        if (running + t > budget && kept.length > 0) break;
        kept.unshift(rest[i]);
        running += t;
    }

    // Never start the kept window on an orphaned tool result.
    while (kept.length > 0 && kept[0].role === 'tool') kept.shift();

    return [...system, ...kept];
}
