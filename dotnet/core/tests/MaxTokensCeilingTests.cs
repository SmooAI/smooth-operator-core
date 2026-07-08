using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Parity tests for the model-output-token ceiling clamp — the C# analog of the Rust core's
/// <c>effective_max_tokens_clamps_to_model_ceiling</c>. A budget <c>max_tokens</c> must never exceed
/// what the model can physically emit (<see cref="AgentOptions.ModelMaxOutputTokens"/>), otherwise a
/// reasoning model burns its budget and returns empty, or the upstream 400s. EPIC th-1cc9fa.
/// </summary>
public class MaxTokensCeilingTests
{
    [Fact]
    public void NoBudget_LeavesMaxTokensUnset()
    {
        // No budget configured → passthrough (don't send max_tokens), regardless of any ceiling.
        Assert.Null(new AgentOptions().EffectiveMaxTokens);
        Assert.Null(new AgentOptions { ModelMaxOutputTokens = 8_192 }.EffectiveMaxTokens);
    }

    [Fact]
    public void NoCeiling_PassesBudgetThrough()
    {
        Assert.Equal(32_768, new AgentOptions { MaxOutputTokens = 32_768 }.EffectiveMaxTokens);
    }

    [Fact]
    public void BudgetAboveCeiling_ClampsDownToCeiling()
    {
        var options = new AgentOptions { MaxOutputTokens = 32_768, ModelMaxOutputTokens = 8_192 };
        Assert.Equal(8_192, options.EffectiveMaxTokens);
    }

    [Fact]
    public void BudgetBelowCeiling_KeepsBudget()
    {
        var options = new AgentOptions { MaxOutputTokens = 32_768, ModelMaxOutputTokens = 384_000 };
        Assert.Equal(32_768, options.EffectiveMaxTokens);
    }

    [Fact]
    public void ZeroOrNegativeCeiling_IsIgnored()
    {
        // A bogus (0 / negative) ceiling must not clamp the budget to nothing — treat it as unknown.
        Assert.Equal(32_768, new AgentOptions { MaxOutputTokens = 32_768, ModelMaxOutputTokens = 0 }.EffectiveMaxTokens);
        Assert.Equal(32_768, new AgentOptions { MaxOutputTokens = 32_768, ModelMaxOutputTokens = -5 }.EffectiveMaxTokens);
    }

    [Fact]
    public void EqualBudgetAndCeiling_ReturnsThatValue()
    {
        Assert.Equal(8_192, new AgentOptions { MaxOutputTokens = 8_192, ModelMaxOutputTokens = 8_192 }.EffectiveMaxTokens);
    }

    [Fact]
    public void NeverClampsToZero()
    {
        // Even a tiny ceiling stays ≥ 1 (a 0 max_tokens is a guaranteed-empty request).
        Assert.Equal(1, new AgentOptions { MaxOutputTokens = 32_768, ModelMaxOutputTokens = 1 }.EffectiveMaxTokens);
    }

    [Fact]
    public async Task Agent_SendsClampedMaxTokensOnTheRequest()
    {
        // End-to-end: the clamped value actually reaches the model call's ChatOptions.MaxOutputTokens.
        var mock = new MockLlmProvider().PushText("ok");
        var agent = new SmoothAgent(mock, new AgentOptions { MaxOutputTokens = 32_768, ModelMaxOutputTokens = 8_192 });

        await agent.RunAsync("hello");

        Assert.Equal(8_192, mock.LastCall!.MaxOutputTokens);
    }

    [Fact]
    public async Task Agent_LeavesMaxTokensUnsetWhenNoBudget()
    {
        var mock = new MockLlmProvider().PushText("ok");
        var agent = new SmoothAgent(mock, new AgentOptions());

        await agent.RunAsync("hello");

        Assert.Null(mock.LastCall!.MaxOutputTokens);
    }
}
