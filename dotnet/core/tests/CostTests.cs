using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Phase-6 parity tests: token + USD cost accounting and budget enforcement.
/// Mirrors the Rust engine's CostTracker / CostBudget / ModelPricing.
/// </summary>
public class CostTests
{
    [Fact]
    public void ModelPricing_ComputesUsd()
    {
        var pricing = new ModelPricing(PromptPerMillionTokens: 3m, CompletionPerMillionTokens: 15m);
        // 1,000,000 prompt + 1,000,000 completion = $3 + $15.
        Assert.Equal(18m, pricing.CostFor(1_000_000, 1_000_000));
    }

    [Fact]
    public void CostTracker_AccumulatesTokensAndCost()
    {
        var tracker = new CostTracker();
        var pricing = new ModelPricing(1m, 2m);
        tracker.Record("m", new UsageDetails { InputTokenCount = 1_000_000, OutputTokenCount = 1_000_000 }, pricing);
        tracker.Record("m", new UsageDetails { InputTokenCount = 1_000_000, OutputTokenCount = 0 }, pricing);

        Assert.Equal(2, tracker.Calls);
        Assert.Equal(3_000_000, tracker.TotalTokens);
        Assert.Equal(1m + 2m + 1m, tracker.TotalCostUsd); // $3 + $1
    }

    [Fact]
    public async Task Agent_TracksCost_WithPricing()
    {
        var mock = new MockLlmProvider().PushText("hi"); // usage 10 in / 5 out
        var options = new AgentOptions();
        options.Pricing[MockLlmProvider.ModelId] = new ModelPricing(PromptPerMillionTokens: 1m, CompletionPerMillionTokens: 2m);
        var agent = new SmoothAgent(mock, options);

        var result = await agent.RunAsync("hello");

        Assert.Equal(1, result.Cost.Calls);
        Assert.Equal(15, result.Cost.TotalTokens);
        // 10 * 1/1e6 + 5 * 2/1e6 = 0.00002
        Assert.Equal(0.00002m, result.Cost.TotalCostUsd);
        Assert.Null(result.BudgetExceeded);
    }

    /// <summary>
    /// Pearl th-df859c: the streaming path used to discard usage and cost entirely — no tracker was
    /// even declared. A streamed turn must now report the SAME totals as the non-streaming one for
    /// the same script, which is what lets the C# server stop hardcoding <c>costUsd: 0</c>.
    /// </summary>
    [Fact]
    public async Task StreamingRun_TracksTheSameUsageAndCostAsRunAsync()
    {
        var options = new AgentOptions();
        options.Pricing[MockLlmProvider.ModelId] = new ModelPricing(PromptPerMillionTokens: 1m, CompletionPerMillionTokens: 2m);

        var streamed = new SmoothAgent(new MockLlmProvider().PushText("hi"), options);
        await foreach (var _ in streamed.RunStreamingAsync("hello"))
        {
        }

        var blocking = await new SmoothAgent(new MockLlmProvider().PushText("hi"), options).RunAsync("hello");

        Assert.NotNull(streamed.LastRunResponse);
        Assert.Equal(blocking.Usage.InputTokenCount, streamed.LastRunResponse!.Usage.InputTokenCount);
        Assert.Equal(blocking.Usage.OutputTokenCount, streamed.LastRunResponse.Usage.OutputTokenCount);
        Assert.Equal(blocking.Cost.TotalTokens, streamed.LastRunResponse.Cost.TotalTokens);
        Assert.Equal(blocking.Cost.TotalCostUsd, streamed.LastRunResponse.Cost.TotalCostUsd);
        Assert.Equal(0.00002m, streamed.LastRunResponse.Cost.TotalCostUsd);
        Assert.Null(streamed.LastRunResponse.BudgetExceeded);
    }

    /// <summary>
    /// The consequence that bit hardest: with no tracker on the streaming path,
    /// <see cref="AgentOptions.Budget"/> was silently inert — a runaway streamed turn could not be
    /// stopped by its own spend ceiling. Same script and budget as
    /// <see cref="TokenBudget_HaltsTheRun"/>, same answer.
    /// </summary>
    [Fact]
    public async Task StreamingRun_TokenBudget_HaltsTheRun()
    {
        var mock = new MockLlmProvider();
        for (var i = 0; i < 10; i++)
        {
            mock.PushToolCall($"c{i}", "noop", new Dictionary<string, object?>());
        }
        var options = new AgentOptions
        {
            MaxIterations = 10,                      // not the limiter here
            Budget = new CostBudget { MaxTokens = 20 },
        };
        options.Tools.Add(AIFunctionFactory.Create(() => "ok", "noop", "does nothing"));
        var agent = new SmoothAgent(mock, options);

        await foreach (var _ in agent.RunStreamingAsync("loop"))
        {
        }

        // Call 1 = 15 tokens (≤ 20, continues); call 2 = 30 tokens (> 20, halts).
        Assert.NotNull(agent.LastRunResponse);
        Assert.Equal(2, agent.LastRunResponse!.Iterations);
        Assert.Equal(30, agent.LastRunResponse.Cost.TotalTokens);
        Assert.NotNull(agent.LastRunResponse.BudgetExceeded);
        Assert.Equal(20, agent.LastRunResponse.BudgetExceeded!.LimitTokens);
    }

    /// <summary>A turn abandoned mid-stream reports nothing, rather than a partial or stale total.</summary>
    [Fact]
    public async Task StreamingRun_AbandonedMidStream_ReportsNoTotals()
    {
        var agent = new SmoothAgent(new MockLlmProvider().PushText("hello there friend"), new AgentOptions());

        await foreach (var _ in agent.RunStreamingAsync("hi"))
        {
            break;
        }

        Assert.Null(agent.LastRunResponse);
    }

    /// <summary>
    /// Pearl th-9520d3: C# was the only engine with no local pricing fallback — an unpriced model
    /// recorded exactly $0 on every call, so a caller could not tell "this model is free" from
    /// "nobody told me the price". Same two entries, same prices, as the Go/Python/TypeScript
    /// engines' <c>DEFAULT_PRICING</c>.
    /// </summary>
    [Fact]
    public void ForModel_FallsBackToTheDefaultTable_AndOverridesWin()
    {
        // A model in the default table is priced with no caller table at all.
        Assert.Equal(new ModelPricing(1.0m, 5.0m), ModelPricing.ForModel("claude-haiku-4-5"));
        Assert.Equal(new ModelPricing(3.0m, 15.0m), ModelPricing.ForModel("claude-sonnet-4-5"));

        // A caller entry wins over the default for the same model.
        var overrides = new Dictionary<string, ModelPricing> { ["claude-haiku-4-5"] = new(99m, 99m) };
        Assert.Equal(new ModelPricing(99m, 99m), ModelPricing.ForModel("claude-haiku-4-5", overrides));

        // Unknown stays unpriced, and so does a null model id.
        Assert.Null(ModelPricing.ForModel("totally-fake-model-name-xyz"));
        Assert.Null(ModelPricing.ForModel(null));
    }

    [Fact]
    public async Task Agent_PricesADefaultTableModel_WithoutAnyCallerPricing()
    {
        // ModelId "claude-haiku-4-5", usage 10 in / 5 out, and AgentOptions.Pricing left EMPTY.
        var response = new ChatResponse(new ChatMessage(ChatRole.Assistant, "hi"))
        {
            Usage = MockLlmProvider.ScriptedUsage(),
            ModelId = "claude-haiku-4-5",
        };
        var agent = new SmoothAgent(new MockLlmProvider().PushResponse(response), new AgentOptions());

        var result = await agent.RunAsync("hello");

        // 10 * 1/1e6 + 5 * 5/1e6 = 0.000035 — before this, exactly 0.
        Assert.Equal(0.000035m, result.Cost.TotalCostUsd);
    }

    [Fact]
    public async Task TokenBudget_HaltsTheRun()
    {
        // The model keeps wanting the tool; each call is 15 tokens. Budget is 20.
        var mock = new MockLlmProvider();
        for (var i = 0; i < 10; i++)
        {
            mock.PushToolCall($"c{i}", "noop", new Dictionary<string, object?>());
        }
        var noop = AIFunctionFactory.Create(() => "ok", "noop", "does nothing");
        var options = new AgentOptions
        {
            MaxIterations = 10,                      // not the limiter here
            Budget = new CostBudget { MaxTokens = 20 },
        };
        options.Tools.Add(noop);
        var agent = new SmoothAgent(mock, options);

        var result = await agent.RunAsync("loop");

        // Call 1 = 15 tokens (≤ 20, continues); call 2 = 30 tokens (> 20, halts).
        Assert.Equal(2, result.Iterations);
        Assert.Equal(30, result.Cost.TotalTokens);
        Assert.NotNull(result.BudgetExceeded);
        Assert.Equal(20, result.BudgetExceeded!.LimitTokens);
    }

    [Fact]
    public async Task CostBudget_HaltsOnUsd()
    {
        var mock = new MockLlmProvider();
        for (var i = 0; i < 10; i++)
        {
            mock.PushToolCall($"c{i}", "noop", new Dictionary<string, object?>());
        }
        var noop = AIFunctionFactory.Create(() => "ok", "noop", "noop");
        var options = new AgentOptions
        {
            MaxIterations = 10,
            // Each call: 10 in * $100/Mtok + 5 out * $100/Mtok = $0.0015. Budget $0.002 → halts after call 2.
            Budget = new CostBudget { MaxCostUsd = 0.002m },
        };
        options.Pricing[MockLlmProvider.ModelId] = new ModelPricing(100m, 100m);
        options.Tools.Add(noop);
        var agent = new SmoothAgent(mock, options);

        var result = await agent.RunAsync("loop");

        Assert.Equal(2, result.Iterations);
        Assert.NotNull(result.BudgetExceeded);
        Assert.True(result.Cost.TotalCostUsd > 0.002m);
    }
}
