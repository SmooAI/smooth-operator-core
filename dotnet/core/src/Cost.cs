using System.Globalization;
using System.Net.Http.Headers;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>Per-model pricing in USD per million tokens. Mirrors the Rust engine's <c>ModelPricing</c>.</summary>
public sealed record ModelPricing(decimal PromptPerMillionTokens, decimal CompletionPerMillionTokens)
{
    public decimal CostFor(long promptTokens, long completionTokens) =>
        (promptTokens * PromptPerMillionTokens / 1_000_000m) +
        (completionTokens * CompletionPerMillionTokens / 1_000_000m);
}

/// <summary>
/// Reads the LLM gateway's authoritative per-request cost from response headers.
/// Mirrors the Rust reference's <c>parse_gateway_cost</c>
/// (<c>rust/smooth-operator-core/src/llm.rs</c>) exactly, candidate list included.
/// </summary>
public static class GatewayCost
{
    /// <summary>
    /// The headers the gateway reports cost in, in PRECEDENCE order. LiteLLM splits
    /// cost across a few: <c>-margin-amount</c> is what the caller actually pays
    /// (includes the gateway's markup), <c>-original</c> is the raw upstream cost,
    /// and the bare <c>x-litellm-response-cost</c> is the legacy shape older versions
    /// emit. The last two are generic fallbacks for other OpenAI-compatible gateways.
    /// </summary>
    public static readonly string[] Headers =
    {
        "x-litellm-response-cost-margin-amount",
        "x-litellm-response-cost-original",
        "x-litellm-response-cost",
        "x-response-cost",
        "x-cost-usd",
    };

    /// <summary>
    /// The FIRST NON-ZERO candidate, or <c>null</c> when every candidate is absent OR
    /// reports zero.
    /// <para>That distinction is the whole point: null means "unmeasured", so the
    /// caller falls back to local <see cref="ModelPricing"/>, whereas locking in 0
    /// would pin cost at zero for the rest of the run. LiteLLM's config on
    /// llm.smoo.ai currently reports 0 for <c>smooth-*</c> aliases on every response,
    /// so taking a zero at face value silently zeroes real spend.</para>
    /// </summary>
    public static decimal? Parse(Func<string, string?> lookup)
    {
        ArgumentNullException.ThrowIfNull(lookup);
        foreach (var name in Headers)
        {
            var raw = lookup(name)?.Trim();
            if (string.IsNullOrEmpty(raw))
            {
                continue;
            }
            // The gateway emits scientific notation (1.47e-05), so Float is required.
            if (decimal.TryParse(raw, NumberStyles.Float, CultureInfo.InvariantCulture, out var cost) && cost > 0m)
            {
                return cost;
            }
        }
        return null;
    }

    /// <summary>Parse from an <see cref="HttpResponseMessage"/>'s headers.</summary>
    public static decimal? Parse(HttpHeaders? headers) =>
        headers is null
            ? null
            : Parse(name => headers.TryGetValues(name, out var values) ? values.FirstOrDefault() : null);

    /// <summary>Parse from a plain header dictionary (case-insensitive on the way in).</summary>
    public static decimal? Parse(IReadOnlyDictionary<string, string?>? headers) =>
        headers is null
            ? null
            : Parse(name => headers.TryGetValue(name, out var v) ? v : null);
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

    /// <summary>
    /// Record one LLM call's usage, preferring the gateway's authoritative cost when
    /// it measured one. A null <paramref name="gatewayCostUsd"/> means "unmeasured"
    /// and falls back to the local <paramref name="pricing"/> estimate — which is the
    /// whole reason <see cref="GatewayCost.Parse(Func{string, string})"/> returns null
    /// rather than 0. Aliased models (<c>smooth-*</c>) price at $0 locally, so the
    /// gateway's number is often the only real one available.
    /// </summary>
    public void RecordWithGatewayCost(string? model, UsageDetails? usage, decimal? gatewayCostUsd, ModelPricing? pricing)
    {
        if (gatewayCostUsd is not { } authoritative)
        {
            Record(model, usage, pricing);
            return;
        }

        var prompt = usage?.InputTokenCount ?? 0;
        var completion = usage?.OutputTokenCount ?? 0;
        TotalPromptTokens += prompt;
        TotalCompletionTokens += completion;
        TotalCostUsd += authoritative;
        _entries.Add(new CostEntry(model, prompt, completion, authoritative));
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
