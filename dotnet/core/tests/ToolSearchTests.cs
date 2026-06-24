using System.Text.Json;
using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Phase-3 parity tests: deferred tools hidden from the model behind the <c>tool_search</c>
/// meta-tool, promoted on demand. Mirrors the Rust reference <c>tool_search.rs</c> and the sibling
/// engines' <c>ToolSearch</c> behaviour.
/// </summary>
public class ToolSearchTests
{
    private static AIFunction DeferredTool(string name, string description, Action? onCall = null) =>
        AIFunctionFactory.Create(() => { onCall?.Invoke(); return $"{name} ran"; }, name, description);

    [Fact]
    public void Search_FuzzyMatchesNameOrDescription_AndPromotes()
    {
        var search = new ToolSearch(new[]
        {
            DeferredTool("git_status", "show the working tree status"),
            DeferredTool("http_get", "make an HTTP request"),
        });

        Assert.True(search.HasDeferred);
        Assert.False(search.IsPromoted("git_status"));

        // Match by description keyword ("http"/"request").
        var json = InvokeSearch(search, "request");
        var doc = JsonDocument.Parse(json).RootElement;

        Assert.Equal(1, doc.GetProperty("matched").GetInt32());
        Assert.True(search.IsPromoted("http_get"));
        Assert.False(search.IsPromoted("git_status"));
    }

    [Fact]
    public void Search_EmptyQuery_PromotesNothing()
    {
        var search = new ToolSearch(new[] { DeferredTool("a", "alpha") });
        var json = InvokeSearch(search, "   ");
        Assert.Equal(0, JsonDocument.Parse(json).RootElement.GetProperty("matched").GetInt32());
        Assert.False(search.IsPromoted("a"));
    }

    [Fact]
    public void Search_CapsAtMaxMatches()
    {
        var tools = Enumerable.Range(0, ToolSearch.MaxMatches + 5)
            .Select(i => DeferredTool($"net_tool_{i}", "network helper"))
            .ToArray();
        var search = new ToolSearch(tools);

        var json = InvokeSearch(search, "network");

        Assert.Equal(ToolSearch.MaxMatches, JsonDocument.Parse(json).RootElement.GetProperty("matched").GetInt32());
        Assert.Equal(ToolSearch.MaxMatches, search.PromotedTools().Count);
    }

    [Fact]
    public async Task Agent_HidesDeferredTool_UntilPromoted()
    {
        var ran = false;
        var deferred = DeferredTool("delete_db", "delete the database", () => ran = true);

        // The model tries to call the deferred tool directly (before discovering it) → unknown
        // tool; then gives up with text.
        var mock = new MockLlmProvider()
            .PushToolCall("c1", "delete_db", new Dictionary<string, object?>())
            .PushText("I cannot find that tool.");

        var options = new AgentOptions();
        options.DeferredTools.Add(deferred);
        var agent = new SmoothAgent(mock, options);

        var result = await agent.RunAsync("delete the database");

        Assert.False(ran); // never dispatched — it wasn't promoted
        Assert.Equal("I cannot find that tool.", result.Text);

        // The model only ever saw tool_search, never the deferred tool's schema.
        var firstCallTools = mock.Calls.Count > 0 ? mock.Recordings[0].Tools : null;
        Assert.NotNull(firstCallTools);
        Assert.Contains(firstCallTools!, t => t is AIFunction f && f.Name == ToolSearch.ToolName);
        Assert.DoesNotContain(firstCallTools!, t => t is AIFunction f && f.Name == "delete_db");
    }

    [Fact]
    public async Task Agent_PromotesViaToolSearch_ThenDispatches()
    {
        var ran = false;
        var deferred = DeferredTool("run_report", "generate the quarterly report", () => ran = true);

        var mock = new MockLlmProvider()
            .PushToolCall("c1", ToolSearch.ToolName, new Dictionary<string, object?> { ["query"] = "report" })
            .PushToolCall("c2", "run_report", new Dictionary<string, object?>())
            .PushText("Report generated.");

        var options = new AgentOptions();
        options.DeferredTools.Add(deferred);
        var agent = new SmoothAgent(mock, options);

        var result = await agent.RunAsync("make the quarterly report");

        Assert.True(ran); // promoted by tool_search, then dispatched
        Assert.Equal("Report generated.", result.Text);

        // By the 2nd model call, the promoted tool's schema had joined the visible set.
        var secondCallTools = mock.Recordings[1].Tools;
        Assert.Contains(secondCallTools!, t => t is AIFunction f && f.Name == "run_report");
    }

    private static string InvokeSearch(ToolSearch search, string query)
    {
        // The meta-tool is an AIFunction; invoke it the way the agent dispatch does.
        var args = new AIFunctionArguments(new Dictionary<string, object?> { ["query"] = query });
        var result = search.MetaTool.InvokeAsync(args).AsTask().GetAwaiter().GetResult();
        return result?.ToString() ?? "{}";
    }
}
