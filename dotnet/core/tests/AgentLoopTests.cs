using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Phase-0 behavioral parity tests for the C# engine. The headline assertion mirrors the
/// Rust core's <c>run_drives_the_loop_via_injected_llm_provider</c>: a text response with
/// no tool calls ends the loop after exactly one model call, and the user's message reached
/// the model.
/// </summary>
public class AgentLoopTests
{
    [Fact]
    public async Task TextResponse_EndsLoopAfterOneCall()
    {
        var mock = new MockLlmProvider().PushText("the answer is 42");
        var agent = new SmoothAgent(mock, new AgentOptions { Instructions = "be helpful" });

        var result = await agent.RunAsync("what is the answer?");

        Assert.Equal("the answer is 42", result.Text);
        Assert.Equal(1, result.Iterations);
        Assert.Equal(1, mock.CallCount);
        // The user's message reached the model.
        Assert.Contains(mock.Calls[0], m => m.Text.Contains("what is the answer?"));
        // The system instructions were prepended.
        Assert.Contains(mock.Calls[0], m => m.Role == ChatRole.System && m.Text.Contains("be helpful"));
        // Usage was accumulated.
        Assert.Equal(15, result.Usage.TotalTokenCount);
    }

    [Fact]
    public async Task ToolCall_IsExecuted_AndResultFedBack()
    {
        var toolFired = false;
        var add = AIFunctionFactory.Create(
            (int a, int b) =>
            {
                toolFired = true;
                return a + b;
            },
            "add",
            "Adds two integers");

        var mock = new MockLlmProvider()
            .PushToolCall("call-1", "add", new Dictionary<string, object?> { ["a"] = 2, ["b"] = 3 })
            .PushText("The sum is 5.");

        var options = new AgentOptions();
        options.Tools.Add(add);
        var agent = new SmoothAgent(mock, options);

        var result = await agent.RunAsync("add 2 and 3");

        Assert.True(toolFired, "the tool should have been executed by the engine");
        Assert.Equal("The sum is 5.", result.Text);
        Assert.Equal(2, result.Iterations);
        Assert.Equal(2, mock.CallCount);
        // The tool result was fed back to the model on the second call.
        Assert.Contains(mock.Calls[1], m => m.Role == ChatRole.Tool);
        // Usage accumulates across both calls.
        Assert.Equal(30, result.Usage.TotalTokenCount);
    }

    [Fact]
    public async Task UnknownTool_ReturnsErrorResult_WithoutThrowing()
    {
        var mock = new MockLlmProvider()
            .PushToolCall("call-1", "does_not_exist", new Dictionary<string, object?>())
            .PushText("sorry, I could not do that");
        var agent = new SmoothAgent(mock, new AgentOptions());

        var result = await agent.RunAsync("do something");

        Assert.Equal("sorry, I could not do that", result.Text);
        Assert.Equal(2, result.Iterations);
        var toolMessage = mock.Calls[1].First(m => m.Role == ChatRole.Tool);
        Assert.Contains(toolMessage.Contents.OfType<FunctionResultContent>(),
            r => r.Result is string s && s.Contains("unknown tool"));
    }

    [Fact]
    public async Task MaxIterations_StopsRunawayToolLoop()
    {
        var mock = new MockLlmProvider();
        for (var i = 0; i < 10; i++)
        {
            mock.PushToolCall($"call-{i}", "noop", new Dictionary<string, object?>());
        }
        var noop = AIFunctionFactory.Create(() => "ok", "noop", "does nothing");
        var options = new AgentOptions { MaxIterations = 3 };
        options.Tools.Add(noop);
        var agent = new SmoothAgent(mock, options);

        var result = await agent.RunAsync("loop forever");

        Assert.Equal(3, result.Iterations);
        Assert.Equal(3, mock.CallCount);
    }

    [Fact]
    public async Task RunStreaming_YieldsTextDeltas()
    {
        var mock = new MockLlmProvider().PushText("hello world");
        var agent = new SmoothAgent(mock, new AgentOptions());

        var streamed = new System.Text.StringBuilder();
        await foreach (var update in agent.RunStreamingAsync("hi"))
        {
            streamed.Append(update.Text);
        }

        Assert.Contains("hello world", streamed.ToString());
    }
}
