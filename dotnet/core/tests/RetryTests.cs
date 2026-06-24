using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Retry-with-exponential-backoff around the model call, driven by the reusable
/// <see cref="MockLlmProvider"/> (it can script a transient error via <c>PushError</c>).
/// <see cref="AgentOptions.RetryBackoff"/> is set to <see cref="TimeSpan.Zero"/> so no real
/// time is spent sleeping.
/// </summary>
public class RetryTests
{
    [Fact]
    public async Task Retries_Then_Succeeds()
    {
        // Errors k times then a text reply; MaxRetries >= k → the turn succeeds and the
        // model is called exactly k+1 times.
        var mock = new MockLlmProvider()
            .PushError("rate limited")
            .PushError("rate limited")
            .PushText("ok");
        var agent = new SmoothAgent(mock, new AgentOptions { MaxRetries = 2, RetryBackoff = TimeSpan.Zero });

        var result = await agent.RunAsync("hi");

        Assert.Equal("ok", result.Text);
        Assert.Equal(3, mock.CallCount); // k+1 = 2 failures + 1 success
    }

    [Fact]
    public async Task Error_Propagates_When_Retries_Exhausted()
    {
        // Errors MaxRetries+1 times → the provider error propagates (the turn fails).
        var mock = new MockLlmProvider()
            .PushError("boom")
            .PushError("boom");
        var agent = new SmoothAgent(mock, new AgentOptions { MaxRetries = 1, RetryBackoff = TimeSpan.Zero });

        var ex = await Assert.ThrowsAsync<InvalidOperationException>(() => agent.RunAsync("hi"));
        Assert.Contains("boom", ex.Message);
        Assert.Equal(2, mock.CallCount); // MaxRetries + 1 attempts
    }

    [Fact]
    public async Task No_Retry_By_Default()
    {
        // Default MaxRetries=0 → a single error propagates immediately (one attempt).
        var mock = new MockLlmProvider()
            .PushError("nope")
            .PushText("never reached");
        var agent = new SmoothAgent(mock, new AgentOptions());

        var ex = await Assert.ThrowsAsync<InvalidOperationException>(() => agent.RunAsync("hi"));
        Assert.Contains("nope", ex.Message);
        Assert.Equal(1, mock.CallCount);
    }
}
