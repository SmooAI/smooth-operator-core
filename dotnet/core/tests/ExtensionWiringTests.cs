using System.Text.Json.Nodes;
using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core.Extensions;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// The SEP seam, driven end-to-end through the real agent loop with a scripted client. These cover
/// the WIRING — that the host is actually reached from the loop — which is exactly what was missing:
/// the extension host was published but unreachable, with zero references from the agent.
///
/// The host itself (fold policy, cross-tool guard, subprocess) is tested in the SEP suites; a stub
/// <see cref="IExtensionHooks"/> keeps these deterministic and subprocess-free. Ported from
/// go/core/extension_seam_test.go so all four engines assert the same behaviour.
/// </summary>
public class ExtensionWiringTests
{
    /// <summary>A scripted <see cref="IExtensionHooks"/> that records what the loop asked it.</summary>
    private sealed class FakeHooks : IExtensionHooks
    {
        public List<AITool> EagerTools { get; } = new();
        public List<AITool> Deferred { get; } = new();
        public string? BlockTool { get; set; }
        public string BlockReason { get; set; } = string.Empty;
        public JsonObject? PatchArgs { get; set; }
        public List<string> Events { get; } = new();
        public Dictionary<string, JsonNode?> Payloads { get; } = new(StringComparer.Ordinal);
        public List<string> HookedTools { get; } = new();

        public IReadOnlyList<AITool> Tools() => EagerTools;

        public IReadOnlyList<AITool> DeferredTools() => Deferred;

        public Task<FoldedHook> RunToolCallHookAsync(string tool, JsonNode arguments)
        {
            HookedTools.Add(tool);
            if (tool == BlockTool)
            {
                return Task.FromResult<FoldedHook>(new FoldedHook.Blocked { Reason = BlockReason });
            }
            var value = new JsonObject { ["tool"] = tool, ["arguments"] = PatchArgs?.DeepClone() ?? arguments.DeepClone() };
            return Task.FromResult<FoldedHook>(new FoldedHook.Proceed { Value = value });
        }

        public void DispatchEvent(string @event, JsonNode? payload)
        {
            Events.Add(@event);
            Payloads[@event] = payload;
        }
    }

    /// <summary>A tool that records the arguments it was executed with.</summary>
    private sealed class RecordingTool
    {
        public int Calls;
        public string? GotCity;

        public AIFunction AsFunction(string name) =>
            AIFunctionFactory.Create((string city) => { Calls++; GotCity = city; return "ok"; }, name);
    }

    private static ChatResponse ToolCall(string callId, string name, Dictionary<string, object?> args) =>
        new(new ChatMessage(ChatRole.Assistant, new List<AIContent> { new FunctionCallContent(callId, name, args) }))
        {
            Usage = new UsageDetails(),
            ModelId = MockLlmProvider.ModelId,
        };

    private static List<string> ToolResults(IEnumerable<ChatMessage> messages) =>
        messages
            .Where(m => m.Role == ChatRole.Tool)
            .SelectMany(m => m.Contents.OfType<FunctionResultContent>())
            .Select(r => r.Result?.ToString() ?? string.Empty)
            .ToList();

    [Fact]
    public async Task ExtensionTools_AreVisibleAndDispatchable()
    {
        var rec = new RecordingTool();
        var hooks = new FakeHooks();
        hooks.EagerTools.Add(rec.AsFunction("weather.forecast"));

        var mock = new MockLlmProvider();
        mock.PushResponse(ToolCall("c1", "weather.forecast", new Dictionary<string, object?> { ["city"] = "NYC" }))
            .PushText("done");

        var options = new AgentOptions { Extensions = hooks };
        var result = await new SmoothAgent(mock, options).RunAsync("go");

        Assert.Equal(1, rec.Calls);
        Assert.Equal("NYC", rec.GotCity);
        Assert.Equal("done", result.Text);
    }

    [Fact]
    public async Task DeferredExtensionTools_StayHiddenUntilPromoted()
    {
        var rec = new RecordingTool();
        var hooks = new FakeHooks();
        hooks.Deferred.Add(rec.AsFunction("weather.forecast"));

        var mock = new MockLlmProvider();
        mock.PushText("done");

        var options = new AgentOptions { Extensions = hooks };
        await new SmoothAgent(mock, options).RunAsync("go");

        // The trap: a deferred extension tool must NOT be advertised. Only tool_search is visible.
        var advertised = mock.LastCall?.Tools?.OfType<AIFunction>().Select(t => t.Name).ToList() ?? new List<string>();
        Assert.DoesNotContain("weather.forecast", advertised);
        Assert.Contains("tool_search", advertised);
    }

    [Fact]
    public async Task ToolCallHookVeto_BlocksExecutionAndTellsTheModelWhy()
    {
        var rec = new RecordingTool();
        var hooks = new FakeHooks { BlockTool = "weather.forecast", BlockReason = "no network today" };
        hooks.EagerTools.Add(rec.AsFunction("weather.forecast"));

        var mock = new MockLlmProvider();
        mock.PushResponse(ToolCall("c1", "weather.forecast", new Dictionary<string, object?> { ["city"] = "NYC" }))
            .PushText("done");

        var options = new AgentOptions { Extensions = hooks };
        var result = await new SmoothAgent(mock, options).RunAsync("go");

        Assert.Equal(0, rec.Calls); // the call never ran
        // Exact wording is a cross-language contract (Rust/Go/TS/C#).
        Assert.Equal(new[] { "error: blocked by extension: no network today" }, ToolResults(result.Messages));
    }

    [Fact]
    public async Task ToolCallHookModify_RewritesArguments()
    {
        var rec = new RecordingTool();
        var hooks = new FakeHooks { PatchArgs = new JsonObject { ["city"] = "Boston" } };
        hooks.EagerTools.Add(rec.AsFunction("weather.forecast"));

        var mock = new MockLlmProvider();
        mock.PushResponse(ToolCall("c1", "weather.forecast", new Dictionary<string, object?> { ["city"] = "NYC" }))
            .PushText("done");

        var options = new AgentOptions { Extensions = hooks };
        await new SmoothAgent(mock, options).RunAsync("go");

        Assert.Equal(1, rec.Calls);
        Assert.Equal("Boston", rec.GotCity); // the hook's rewrite is what actually executed
    }

    [Fact]
    public async Task TurnEvents_AreDispatchedInOrderWithPayloads()
    {
        var hooks = new FakeHooks();
        var mock = new MockLlmProvider();
        mock.PushText("hello");

        var options = new AgentOptions { Name = "wiring-agent", Extensions = hooks };
        await new SmoothAgent(mock, options).RunAsync("go");

        Assert.Equal(new[] { "turn_start", "message_end", "turn_end" }, hooks.Events);
        Assert.Equal("wiring-agent", hooks.Payloads["turn_start"]!["agent_id"]!.GetValue<string>());
        Assert.Equal(1, hooks.Payloads["message_end"]!["iteration"]!.GetValue<int>());
        Assert.Equal("hello", hooks.Payloads["message_end"]!["content"]!.GetValue<string>());
        Assert.Equal("wiring-agent", hooks.Payloads["turn_end"]!["agent_id"]!.GetValue<string>());
        Assert.Equal(1, hooks.Payloads["turn_end"]!["iterations"]!.GetValue<int>());
    }

    [Fact]
    public async Task TurnEvents_FireOnMaxIterationExit()
    {
        var rec = new RecordingTool();
        var hooks = new FakeHooks();
        hooks.EagerTools.Add(rec.AsFunction("weather.forecast"));

        // The model keeps asking for tools; the iteration cap is what ends the turn.
        var mock = new MockLlmProvider();
        for (var i = 0; i < 4; i++)
        {
            mock.PushResponse(ToolCall($"c{i}", "weather.forecast", new Dictionary<string, object?> { ["city"] = "NYC" }));
        }

        var options = new AgentOptions { MaxIterations = 2, Extensions = hooks };
        await new SmoothAgent(mock, options).RunAsync("go");

        // A non-happy-path exit must still close the turn out.
        Assert.Equal(new[] { "turn_start", "message_end", "turn_end" }, hooks.Events);
        Assert.Equal(2, hooks.Payloads["turn_end"]!["iterations"]!.GetValue<int>());
    }

    [Fact]
    public async Task TurnEvents_FireOnBudgetExit()
    {
        var hooks = new FakeHooks();
        var mock = new MockLlmProvider();
        mock.PushText("spendy");

        var options = new AgentOptions
        {
            Extensions = hooks,
            Budget = new CostBudget { MaxCostUsd = 0.0000001m },
        };
        options.Pricing[MockLlmProvider.ModelId] = new ModelPricing(1000m, 1000m);

        var result = await new SmoothAgent(mock, options).RunAsync("go");

        Assert.NotNull(result.BudgetExceeded); // the budget really did trip
        Assert.Equal(new[] { "turn_start", "message_end", "turn_end" }, hooks.Events);
    }

    [Fact]
    public async Task StreamingTurn_EmitsTheSameEvents()
    {
        var hooks = new FakeHooks();
        var mock = new MockLlmProvider();
        mock.PushText("hello");

        var options = new AgentOptions { Name = "wiring-agent", Extensions = hooks };
        await foreach (var _ in new SmoothAgent(mock, options).RunStreamingAsync("go"))
        {
            // drain
        }

        // Stream parity: the streaming path is not a second, quieter loop.
        Assert.Equal(new[] { "turn_start", "message_end", "turn_end" }, hooks.Events);
        Assert.Equal("hello", hooks.Payloads["message_end"]!["content"]!.GetValue<string>());
    }

    [Fact]
    public async Task NoExtensionHost_LeavesTheLoopUnchanged()
    {
        var rec = new RecordingTool();
        var mock = new MockLlmProvider();
        mock.PushResponse(ToolCall("c1", "forecast", new Dictionary<string, object?> { ["city"] = "NYC" }))
            .PushText("done");

        var options = new AgentOptions(); // Extensions left null
        options.Tools.Add(rec.AsFunction("forecast"));
        var result = await new SmoothAgent(mock, options).RunAsync("go");

        Assert.Equal(1, rec.Calls);
        Assert.Equal("NYC", rec.GotCity);
        Assert.Equal("done", result.Text);
    }

    [Fact]
    public void SharedOptions_DoNotAccumulateExtensionTools()
    {
        // AgentOptions is a shared object (unlike Go's value copy), so merging the host's tools must
        // not write back onto it — two agents over one options would otherwise double-add.
        var hooks = new FakeHooks();
        hooks.EagerTools.Add(new RecordingTool().AsFunction("weather.forecast"));
        var options = new AgentOptions { Extensions = hooks };

        _ = new SmoothAgent(new MockLlmProvider(), options);
        _ = new SmoothAgent(new MockLlmProvider(), options);

        Assert.Empty(options.Tools);
        Assert.Empty(options.DeferredTools);
    }
}
