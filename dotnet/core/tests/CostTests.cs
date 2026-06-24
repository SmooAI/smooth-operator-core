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
