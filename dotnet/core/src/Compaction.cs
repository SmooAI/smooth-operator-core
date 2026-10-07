using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// How to shrink a conversation when it exceeds <see cref="AgentOptions.MaxContextTokens"/>.
/// Mirrors the Rust engine's <c>CompactionStrategy</c> (more variants — summarize, layered —
/// arrive in later phases).
/// </summary>
public enum CompactionStrategy
{
    /// <summary>Never compact; let the conversation grow unbounded.</summary>
    None,

    /// <summary>
    /// Drop the oldest non-system messages until the conversation fits the budget,
    /// always preserving the system prompt and the most recent user message.
    /// </summary>
    SlidingWindow,
}

/// <summary>What a compaction pass did (for events/telemetry/tests).</summary>
public readonly record struct CompactionResult(int MessagesRemoved, int TokensBefore, int TokensAfter)
{
    public bool Compacted => MessagesRemoved > 0;
}

/// <summary>
/// Conversation-window management. Token counts are a deliberately cheap heuristic
/// (~4 chars/token) for Phase 1 — behavior (what gets kept/dropped), not exact counts,
/// is what the parity tests assert. A real tokenizer can be swapped in later without
/// changing the contract.
/// </summary>
internal static class Compactor
{
    /// <summary>Rough token estimate for a single message.</summary>
    public static int EstimateTokens(ChatMessage message) => (message.Text.Length / 4) + 4;

    /// <summary>Rough token estimate for a whole conversation.</summary>
    public static int EstimateTokens(IEnumerable<ChatMessage> messages) => messages.Sum(EstimateTokens);

    /// <summary>
    /// Compact <paramref name="messages"/> in place to fit <paramref name="maxTokens"/> under the
    /// given <paramref name="strategy"/>. Preserves the leading system message (if any) and the
    /// final message (the live user turn), and drops an assistant tool call only together
    /// with its tool results. Returns what it did.
    /// </summary>
    public static CompactionResult Compact(List<ChatMessage> messages, CompactionStrategy strategy, int maxTokens)
    {
        var before = EstimateTokens(messages);
        if (strategy == CompactionStrategy.None || before <= maxTokens || messages.Count <= 2)
        {
            return new CompactionResult(0, before, before);
        }

        // Oldest droppable index: 1 if a system message leads, else 0. Never drop the last message.
        var hasSystem = messages.Count > 0 && messages[0].Role == ChatRole.System;
        var firstDroppable = hasSystem ? 1 : 0;
        var removed = 0;

        while (EstimateTokens(messages) > maxTokens)
        {
            // Drop the oldest message together with the tool results that follow it, so a
            // tool result never outlives the assistant tool call it answers — the provider
            // 400s on an orphan ("No tool call found for function call output", SMOODEV-3704).
            var span = 1;
            while (firstDroppable + span < messages.Count && messages[firstDroppable + span].Role == ChatRole.Tool)
            {
                span++;
            }

            // Never drop the final message (the live turn).
            if (messages.Count - firstDroppable - span < 1)
            {
                break;
            }

            messages.RemoveRange(firstDroppable, span);
            removed += span;
        }

        return new CompactionResult(removed, before, EstimateTokens(messages));
    }
}
