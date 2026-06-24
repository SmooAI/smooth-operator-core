using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Phase-5 parity tests: the cast (roles + clearance) and subagent dispatch — a lead delegates
/// to a scoped sidekick whose transcript stays isolated. Mirrors the Rust engine's Cast +
/// DispatchSubagentTool.
/// </summary>
public class CastTests
{
    private static (AIFunction tool, Func<bool> fired) Destructive(string name)
    {
        var fired = false;
        var tool = AIFunctionFactory.Create(() => { fired = true; return "done"; }, name, "destructive");
        return (tool, () => fired);
    }

    [Fact]
    public void Clearance_AllowDenyRules()
    {
        Assert.True(Clearance.AllowAll().Allows("anything"));
        Assert.False(Clearance.DenyAll().Allows("anything"));

        var allow = Clearance.Allow("a", "b");
        Assert.True(allow.Allows("a"));
        Assert.False(allow.Allows("c"));

        var deny = Clearance.Deny("x");
        Assert.False(deny.Allows("x"));
        Assert.True(deny.Allows("y"));
    }

    [Fact]
    public void Cast_RegisterAndQuery()
    {
        var cast = new Cast()
            .Register(new OperatorRole("researcher", RoleKind.Sidekick, "research things"))
            .Register(new OperatorRole("critic", RoleKind.Shadow, "critique") { Hidden = true });

        Assert.Equal(2, cast.Count);
        Assert.NotNull(cast.Get("researcher"));
        Assert.Single(cast.Sidekicks());     // only the researcher
        Assert.Single(cast.ListVisible());   // critic is hidden
    }

    [Fact]
    public async Task Lead_DispatchesSidekick_SummaryBubblesUp_TranscriptIsolated()
    {
        var cast = new Cast().Register(new OperatorRole("researcher", RoleKind.Sidekick, "you research things"));

        var mock = new MockLlmProvider()
            .PushToolCall("c1", SubagentDispatcher.ToolName, new Dictionary<string, object?> { ["role"] = "researcher", ["task"] = "research the return window" })
            .PushText("The return window is 17 days.")  // the sidekick's answer
            .PushText("It's 17 days.");                  // the lead's final answer

        var dispatcher = new SubagentDispatcher(mock, cast);
        var options = new AgentOptions { Instructions = "you are the lead" };
        options.Tools.Add(dispatcher.AsTool());
        var lead = new SmoothAgent(mock, options);

        var result = await lead.RunAsync("How long is the return window?");

        Assert.Equal("It's 17 days.", result.Text);
        Assert.Equal(3, mock.CallCount); // lead → sidekick → lead

        // The sidekick saw its task...
        Assert.Contains(mock.Calls[1], m => m.Text.Contains("research the return window"));
        // ...but the lead never saw the sidekick's internal prompt — only the summary it returned,
        // which arrives as the dispatch tool's result content (not message text).
        Assert.DoesNotContain(mock.Calls[2], m => m.Text.Contains("research the return window"));
        Assert.Contains(
            mock.Calls[2].SelectMany(m => m.Contents).OfType<FunctionResultContent>(),
            r => r.Result?.ToString()?.Contains("17 days") == true);
    }

    [Fact]
    public async Task Sidekick_CannotCallDeniedTool_ClearanceFiltersItOut()
    {
        var (search, _) = Destructive("search");
        var (delete, deleteFired) = Destructive("delete");

        // The sidekick's clearance denies "delete".
        var cast = new Cast().Register(
            new OperatorRole("limited", RoleKind.Sidekick, "you tidy up") { Permissions = Clearance.Deny("delete") });

        var mock = new MockLlmProvider()
            .PushToolCall("c1", SubagentDispatcher.ToolName, new Dictionary<string, object?> { ["role"] = "limited", ["task"] = "clean up" })
            .PushToolCall("c2", "delete", new Dictionary<string, object?>())  // sidekick tries the denied tool
            .PushText("I couldn't delete anything.")                          // after it comes back unknown
            .PushText("Done — nothing was deleted.");                         // lead's final answer

        var dispatcher = new SubagentDispatcher(mock, cast, new List<AITool> { search, delete });
        var options = new AgentOptions { Instructions = "lead" };
        options.Tools.Add(dispatcher.AsTool());
        var lead = new SmoothAgent(mock, options);

        await lead.RunAsync("tidy things up");

        Assert.False(deleteFired(), "a tool denied by clearance must not be reachable by the sidekick");
    }
}
