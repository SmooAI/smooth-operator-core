using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Tests for concurrent (parallel) tool-call execution. When
/// <see cref="AgentOptions.ParallelToolCalls"/> is true and an assistant turn returns ≥2 tool
/// calls, the dispatches run concurrently (<c>Task.WhenAll</c>) — but the tool-result contents
/// must still be assembled in the original call order so the transcript is deterministic.
/// Default (false) keeps sequential dispatch.
/// </summary>
public class ParallelToolsTests
{
    private static ChatResponse MultiToolCall(params (string CallId, string Name)[] calls)
    {
        var contents = calls
            .Select(c => (AIContent)new FunctionCallContent(c.CallId, c.Name, new Dictionary<string, object?>()))
            .ToList();
        return new ChatResponse(new ChatMessage(ChatRole.Assistant, contents))
        {
            Usage = new UsageDetails(),
            ModelId = MockLlmProvider.ModelId,
        };
    }

    private static List<string> ToolResults(IList<ChatMessage> messages) =>
        messages
            .Where(m => m.Role == ChatRole.Tool)
            .SelectMany(m => m.Contents.OfType<FunctionResultContent>())
            .Select(r => r.Result?.ToString() ?? string.Empty)
            .ToList();

    [Fact]
    public async Task ParallelDispatch_Overlaps()
    {
        // Two tools that each block until both have started — only completes if concurrent.
        var bothStarted = new TaskCompletionSource();
        var started = 0;
        var gate = new object();
        AIFunction Slow(string name) => AIFunctionFactory.Create(
            async () =>
            {
                lock (gate)
                {
                    if (++started == 2)
                    {
                        bothStarted.TrySetResult();
                    }
                }
                await bothStarted.Task;
                return name;
            },
            name);

        var mock = new MockLlmProvider();
        mock.PushResponse(MultiToolCall(("c1", "a"), ("c2", "b"))).PushText("done");
        var options = new AgentOptions { ParallelToolCalls = true };
        options.Tools.Add(Slow("a"));
        options.Tools.Add(Slow("b"));
        var agent = new SmoothAgent(mock, options);

        var run = agent.RunAsync("go");
        var finished = await Task.WhenAny(run, Task.Delay(TimeSpan.FromSeconds(3)));
        Assert.Same(run, finished); // didn't time out → ran concurrently
        var result = await run;
        Assert.Equal("done", result.Text);
    }

    [Fact]
    public async Task OrderPreserved_DespiteScrambledCompletion()
    {
        var gates = new Dictionary<string, TaskCompletionSource>
        {
            ["A"] = new(),
            ["B"] = new(),
            ["C"] = new(),
        };
        AIFunction Make(string name) => AIFunctionFactory.Create(
            async () =>
            {
                await gates[name].Task;
                return $"result-{name}";
            },
            name);

        var mock = new MockLlmProvider();
        mock.PushResponse(MultiToolCall(("c1", "A"), ("c2", "B"), ("c3", "C"))).PushText("done");
        var options = new AgentOptions { ParallelToolCalls = true };
        options.Tools.Add(Make("A"));
        options.Tools.Add(Make("B"));
        options.Tools.Add(Make("C"));
        var agent = new SmoothAgent(mock, options);

        var run = agent.RunAsync("go");
        // Finish in B, C, A order — opposite of transcript order for A.
        await Task.Delay(20);
        gates["B"].SetResult();
        await Task.Delay(20);
        gates["C"].SetResult();
        await Task.Delay(20);
        gates["A"].SetResult();
        await run;

        var results = ToolResults(mock.Calls[1]);
        Assert.Equal(new[] { "result-A", "result-B", "result-C" }, results);
    }

    [Fact]
    public async Task FailingTool_KeepsItsPosition()
    {
        AIFunction Ok(string name) => AIFunctionFactory.Create(() => "ok", name);
        var boom = AIFunctionFactory.Create(
            string () => throw new InvalidOperationException("kaboom"),
            "B");

        var mock = new MockLlmProvider();
        mock.PushResponse(MultiToolCall(("c1", "A"), ("c2", "B"), ("c3", "C"))).PushText("done");
        var options = new AgentOptions { ParallelToolCalls = true };
        options.Tools.Add(Ok("A"));
        options.Tools.Add(boom);
        options.Tools.Add(Ok("C"));
        var agent = new SmoothAgent(mock, options);

        await agent.RunAsync("go");

        var results = ToolResults(mock.Calls[1]);
        Assert.Equal(3, results.Count);
        Assert.Equal("ok", results[0]);
        Assert.Contains("kaboom", results[1]);
        Assert.Equal("ok", results[2]);
    }

    [Fact]
    public async Task DefaultOff_DispatchesSequentially()
    {
        var order = new List<string>();
        var orderLock = new object();
        AIFunction Make(string name) => AIFunctionFactory.Create(
            () =>
            {
                lock (orderLock)
                {
                    order.Add(name);
                }
                return name;
            },
            name);

        var mock = new MockLlmProvider();
        mock.PushResponse(MultiToolCall(("c1", "A"), ("c2", "B"))).PushText("done");
        var options = new AgentOptions(); // ParallelToolCalls defaults false
        options.Tools.Add(Make("A"));
        options.Tools.Add(Make("B"));
        var agent = new SmoothAgent(mock, options);

        await agent.RunAsync("go");

        Assert.Equal(new[] { "A", "B" }, order);
    }

    [Fact]
    public async Task SingleToolCall_IdenticalWithFlagOn()
    {
        var fired = false;
        var add = AIFunctionFactory.Create(
            (int a, int b) =>
            {
                fired = true;
                return a + b;
            },
            "add");

        var mock = new MockLlmProvider()
            .PushToolCall("c1", "add", new Dictionary<string, object?> { ["a"] = 2, ["b"] = 3 })
            .PushText("done");
        var options = new AgentOptions { ParallelToolCalls = true };
        options.Tools.Add(add);
        var agent = new SmoothAgent(mock, options);

        var result = await agent.RunAsync("go");
        Assert.True(fired);
        Assert.Equal("done", result.Text);
    }
}
