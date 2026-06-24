using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Phase-3 parity tests: the <see cref="MockLlmProvider"/> record/replay double over the
/// <see cref="IChatClient"/> provider seam — scripted FIFO outcomes (text / tool-call / error) and
/// request recording. Mirrors the Rust reference's <c>MockLlmClient</c> and the sibling engines'
/// <c>MockLlmProvider</c>.
/// </summary>
public class LlmProviderTests
{
    [Fact]
    public async Task ScriptedText_DrivesAgent_AndRecordsTheCall()
    {
        var mock = new MockLlmProvider().PushText("hello there");
        var agent = new SmoothAgent(mock, new AgentOptions());

        var result = await agent.RunAsync("hi");

        Assert.Equal("hello there", result.Text);
        Assert.Equal(1, mock.CallCount);
        Assert.NotNull(mock.LastCall);
        Assert.Contains(mock.LastCall!.Messages, m => m.Text.Contains("hi"));
    }

    [Fact]
    public async Task PushError_ThrowsOnThatCall()
    {
        var mock = new MockLlmProvider().PushError("rate limited");
        var agent = new SmoothAgent(mock, new AgentOptions());

        var ex = await Assert.ThrowsAsync<InvalidOperationException>(() => agent.RunAsync("hi"));
        Assert.Contains("rate limited", ex.Message);
    }

    [Fact]
    public async Task FifoOrder_ToolCallThenText()
    {
        var fired = false;
        var tool = AIFunctionFactory.Create(() => { fired = true; return "42"; }, "answer", "returns the answer");

        var mock = new MockLlmProvider()
            .PushToolCall("c1", "answer", new Dictionary<string, object?>())
            .PushText("the answer is 42");
        var options = new AgentOptions();
        options.Tools.Add(tool);
        var agent = new SmoothAgent(mock, options);

        var result = await agent.RunAsync("what's the answer?");

        Assert.True(fired);
        Assert.Equal("the answer is 42", result.Text);
        Assert.Equal(2, mock.CallCount);
    }

    [Fact]
    public async Task RecordsToolsOffered_ToTheModel()
    {
        var tool = AIFunctionFactory.Create(() => "ok", "ping", "ping the server");
        var mock = new MockLlmProvider().PushText("done");
        var options = new AgentOptions();
        options.Tools.Add(tool);

        await new SmoothAgent(mock, options).RunAsync("go");

        Assert.NotNull(mock.LastCall!.Tools);
        Assert.Contains(mock.LastCall.Tools!, t => t is AIFunction f && f.Name == "ping");
    }

    [Fact]
    public async Task EmptyScript_ThrowsRatherThanHang()
    {
        var mock = new MockLlmProvider();
        await Assert.ThrowsAsync<InvalidOperationException>(() => new SmoothAgent(mock, new AgentOptions()).RunAsync("hi"));
    }
}
