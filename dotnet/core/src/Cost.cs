using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>Per-model pricing in USD per million tokens. Mirrors the Rust engine's <c>ModelPricing</c>.</summary>
public sealed record ModelPricing(decimal PromptPerMillionTokens, decimal CompletionPerMillionTokens)
{
    public decimal CostFor(long promptTokens, long completionTokens) =>
        (promptTokens * PromptPerMillionTokens / 1_000_000m) +
        (completionTokens * CompletionPerMillionTokens / 1_000_000m);
}

/// <summary>A spend ceiling for a run. A null limit means "unbounded on that axis".</summary>
public sealed record CostBudget
{
    public decimal? MaxCostUsd { get; init; }

    public long? MaxTokens { get; init; }
}

/// <summary>One recorded LLM call's cost. Mirrors the Rust engine's <c>CostEntry</c>.</summary>
public sealed record CostEntry(string? Model, long PromptTokens, long CompletionTokens, decimal CostUsd);

/// <summary>Details of a budget breach. Mirrors the Rust engine's <c>BudgetExceeded</c>.</summary>
public sealed record BudgetExceeded(decimal SpentUsd, decimal? LimitUsd, long TotalTokens, long? LimitTokens);

/// <summary>
/// Token + USD accounting across a run. Mirrors the Rust engine's <c>CostTracker</c>: every LLM
/// call is recorded, and a <see cref="CostBudget"/> can be checked to stop a runaway agent.
/// </summary>
public sealed class CostTracker
{
    private readonly List<CostEntry> _entries = new();

    public long TotalPromptTokens { get; private set; }

    public long TotalCompletionTokens { get; private set; }

    public long TotalTokens => TotalPromptTokens + TotalCompletionTokens;

    public decimal TotalCostUsd { get; private set; }

    public int Calls => _entries.Count;

    public IReadOnlyList<CostEntry> Entries => _entries;

    /// <summary>Record one LLM call's usage (cost is 0 when no pricing is known for the model).</summary>
    public void Record(string? model, UsageDetails? usage, ModelPricing? pricing)
    {
        var prompt = usage?.InputTokenCount ?? 0;
        var completion = usage?.OutputTokenCount ?? 0;
        var cost = pricing?.CostFor(prompt, completion) ?? 0m;

        TotalPromptTokens += prompt;
        TotalCompletionTokens += completion;
        TotalCostUsd += cost;
        _entries.Add(new CostEntry(model, prompt, completion, cost));
    }

    /// <summary>True if the run has exceeded <paramref name="budget"/> on either axis.</summary>
    public bool ExceedsBudget(CostBudget budget, out BudgetExceeded? exceeded)
    {
        if (budget.MaxCostUsd is { } maxCost && TotalCostUsd > maxCost)
        {
            exceeded = new BudgetExceeded(TotalCostUsd, maxCost, TotalTokens, budget.MaxTokens);
            return true;
        }
        if (budget.MaxTokens is { } maxTokens && TotalTokens > maxTokens)
        {
            exceeded = new BudgetExceeded(TotalCostUsd, budget.MaxCostUsd, TotalTokens, maxTokens);
            return true;
        }
        exceeded = null;
        return false;
    }
}
