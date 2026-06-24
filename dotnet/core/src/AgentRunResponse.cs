using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// The terminal result of a <c>SmoothAgent.RunAsync</c> call: the full message
/// transcript of the turn (including any tool calls + results), accumulated token usage,
/// and how many loop iterations it took. Analogous to the Rust engine returning the
/// final <c>Conversation</c> plus its <c>CostTracker</c>.
/// </summary>
public sealed class AgentRunResponse
{
    public AgentRunResponse(IReadOnlyList<ChatMessage> messages, UsageDetails usage, int iterations, CostTracker cost, BudgetExceeded? budgetExceeded = null)
    {
        Messages = messages;
        Usage = usage;
        Iterations = iterations;
        Cost = cost;
        BudgetExceeded = budgetExceeded;
    }

    /// <summary>Every message produced during the turn, in order.</summary>
    public IReadOnlyList<ChatMessage> Messages { get; }

    /// <summary>Token usage accumulated across all LLM calls in the turn.</summary>
    public UsageDetails Usage { get; }

    /// <summary>Number of LLM calls (loop iterations) the turn took.</summary>
    public int Iterations { get; }

    /// <summary>Token + USD accounting for the turn.</summary>
    public CostTracker Cost { get; }

    /// <summary>Set when the turn stopped early because it hit <see cref="AgentOptions.Budget"/>.</summary>
    public BudgetExceeded? BudgetExceeded { get; }

    /// <summary>The final assistant text — the answer the user sees.</summary>
    public string Text =>
        Messages.LastOrDefault(m => m.Role == ChatRole.Assistant)?.Text ?? string.Empty;
}
