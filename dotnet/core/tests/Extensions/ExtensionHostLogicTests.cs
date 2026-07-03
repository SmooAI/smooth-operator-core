using System.Text.Json.Nodes;
using SmooAI.SmoothOperator.Core.Extensions;

namespace SmooAI.SmoothOperator.Core.Tests.Extensions;

/// <summary>The pure, security-critical host logic — <see cref="ExtensionHost.FoldHookChain"/>,
/// <see cref="ExtensionHost.ValidateCommandContext"/>, <see cref="ExtensionHost.EffectiveSubscriptions"/>,
/// hook-type policy, and the headless <see cref="HostDelegate"/> defaults — plus the empty-host
/// passthrough. Mirrors the Rust <c>extension::host</c> tests, exhaustively for the fold + guard.</summary>
[Collection("SepEnv")]
public sealed class ExtensionHostLogicTests
{
    private static HookStep Replied(HookOutcome o) => new HookStep.Replied { Outcome = o };
    private static HookStep Failed() => new HookStep.Failed();

    // ---- effective subscriptions ----

    [Fact]
    public void EffectiveSubscriptionsIntersectsOrPassesThrough()
    {
        Assert.Equal(
            new HashSet<string> { "turn_start", "turn_end" },
            ExtensionHost.EffectiveSubscriptions(Array.Empty<string>(), new[] { "turn_start", "turn_end" }));
        Assert.Equal(
            new HashSet<string> { "turn_start" },
            ExtensionHost.EffectiveSubscriptions(new[] { "turn_start" }, new[] { "turn_start", "tool_call" }));
        Assert.Single(ExtensionHost.EffectiveSubscriptions(new[] { "turn_start", "turn_end" }, new[] { "turn_end" }));
    }

    // ---- hook type policy + timeout ----

    [Fact]
    public void HookTypeFailPolicyAndTimeout()
    {
        Assert.True(HookType.ToolCall.FailClosed());
        Assert.True(HookType.UserBash.FailClosed());
        Assert.False(HookType.ToolResult.FailClosed());
        Assert.False(HookType.MessageEnd.FailClosed());
        Assert.Equal(TimeSpan.FromSeconds(60), HookType.ToolCall.DefaultTimeout());
        Assert.Equal(TimeSpan.FromSeconds(5), HookType.ToolResult.DefaultTimeout());
        Assert.Equal(HookType.BeforeAgentStart, HookTypeExtensions.FromName("before_agent_start"));
        Assert.Null(HookTypeExtensions.FromName("nope"));
    }

    // ---- fold_hook_chain, exhaustively ----

    private static void AssertProceed(FoldedHook folded, JsonNode expected)
    {
        var p = Assert.IsType<FoldedHook.Proceed>(folded);
        Assert.Equal(expected.ToJsonString(), p.Value.ToJsonString());
    }

    [Fact]
    public void FoldEmptyChainProceedsUnchanged()
    {
        var input = new JsonObject { ["tool"] = "rm" };
        AssertProceed(ExtensionHost.FoldHookChain(HookType.ToolCall, input, Array.Empty<HookStep>()), input);
    }

    [Fact]
    public void FoldContinueKeepsValue()
    {
        var steps = new[] { Replied(new HookOutcome.Continue()), Replied(new HookOutcome.Continue()) };
        AssertProceed(ExtensionHost.FoldHookChain(HookType.ToolResult, new JsonObject { ["a"] = 1 }, steps), new JsonObject { ["a"] = 1 });
    }

    [Fact]
    public void FoldModifyThreadsPatchToNext()
    {
        var steps = new[]
        {
            Replied(new HookOutcome.Modify { Patch = new JsonObject { ["a"] = 2 } }),
            Replied(new HookOutcome.Continue()),
        };
        AssertProceed(ExtensionHost.FoldHookChain(HookType.Context, new JsonObject { ["a"] = 1 }, steps), new JsonObject { ["a"] = 2 });
    }

    [Fact]
    public void FoldBlockShortCircuits()
    {
        var steps = new[]
        {
            Replied(new HookOutcome.Block { Reason = "rm -rf blocked" }),
            Replied(new HookOutcome.Modify { Patch = new JsonObject { ["should"] = "not apply" } }),
        };
        var b = Assert.IsType<FoldedHook.Blocked>(ExtensionHost.FoldHookChain(HookType.ToolCall, new JsonObject(), steps));
        Assert.Equal("rm -rf blocked", b.Reason);
    }

    [Fact]
    public void FoldBlockWithoutReasonGetsDefault()
    {
        var steps = new[] { Replied(new HookOutcome.Block()) };
        var b = Assert.IsType<FoldedHook.Blocked>(ExtensionHost.FoldHookChain(HookType.UserBash, new JsonObject(), steps));
        Assert.Equal("blocked by user_bash hook", b.Reason);
    }

    [Fact]
    public void FoldFailureIsFailClosedForToolCall()
    {
        var b = Assert.IsType<FoldedHook.Blocked>(ExtensionHost.FoldHookChain(HookType.ToolCall, new JsonObject(), new[] { Failed() }));
        Assert.Contains("fail-closed", b.Reason);
    }

    [Fact]
    public void FoldFailureIsFailOpenForOthers()
    {
        var steps = new[] { Failed(), Replied(new HookOutcome.Continue()) };
        AssertProceed(ExtensionHost.FoldHookChain(HookType.ToolResult, new JsonObject { ["x"] = 9 }, steps), new JsonObject { ["x"] = 9 });
    }

    [Fact]
    public void FoldModifyThenFailureFailOpenKeepsPatch()
    {
        var steps = new[] { Replied(new HookOutcome.Modify { Patch = new JsonObject { ["x"] = 2 } }), Failed() };
        AssertProceed(ExtensionHost.FoldHookChain(HookType.Input, new JsonObject { ["x"] = 1 }, steps), new JsonObject { ["x"] = 2 });
    }

    // ---- the command-tier deadlock guard, exhaustively ----

    private static JsonNode Ctx(string tier, string token) =>
        new JsonObject { ["context"] = new JsonObject { ["tier"] = tier, ["token"] = token }, ["text"] = "hi" };

    [Fact]
    public void TokenEpochParsesOrNone()
    {
        Assert.Equal(7, ExtensionHost.TokenEpoch("epoch-7"));
        Assert.Equal(0, ExtensionHost.TokenEpoch("epoch-0"));
        Assert.Null(ExtensionHost.TokenEpoch("epoch-"));
        Assert.Null(ExtensionHost.TokenEpoch("7"));
        Assert.Null(ExtensionHost.TokenEpoch("nonce-3"));
    }

    [Fact]
    public void ValidateCommandContextAcceptsCurrentCommandTier() =>
        ExtensionHost.ValidateCommandContext(Ctx("command", "epoch-4"), 4); // must not throw

    [Fact]
    public void ValidateCommandContextRejectsEventTier()
    {
        var e = Assert.Throws<RpcException>(() => ExtensionHost.ValidateCommandContext(Ctx("event", "epoch-4"), 4));
        Assert.Equal(SepCodes.ContextViolation, e.Error.Code);
    }

    [Fact]
    public void ValidateCommandContextRejectsStaleEpoch()
    {
        var e = Assert.Throws<RpcException>(() => ExtensionHost.ValidateCommandContext(Ctx("command", "epoch-4"), 5));
        Assert.Equal(SepCodes.ContextViolation, e.Error.Code);
    }

    [Fact]
    public void ValidateCommandContextRejectsMissingAndMalformed()
    {
        Assert.Equal(SepCodes.ContextViolation, Assert.Throws<RpcException>(() => ExtensionHost.ValidateCommandContext(new JsonObject { ["text"] = "hi" }, 1)).Error.Code);
        Assert.Equal(SepCodes.ContextViolation, Assert.Throws<RpcException>(() => ExtensionHost.ValidateCommandContext(Ctx("command", "garbage"), 1)).Error.Code);
    }

    // ---- HostDelegate defaults ----

    [Fact]
    public async Task DefaultDelegateUiIsNoUi()
    {
        var e = await Assert.ThrowsAsync<RpcException>(() => new DefaultHostDelegate().UiRequestAsync("ext", new JsonObject { ["kind"] = "confirm" }));
        Assert.Equal(SepCodes.NoUi, e.Error.Code);
    }

    [Fact]
    public async Task DefaultDelegateExecDenied()
    {
        var e = await Assert.ThrowsAsync<RpcException>(() => new DefaultHostDelegate().ExecRunAsync("ext", new JsonObject { ["command"] = "ls" }));
        Assert.Equal(SepCodes.NotTrusted, e.Error.Code);
    }

    [Fact]
    public async Task DefaultDelegateKvRoundtrips()
    {
        var tmp = Directory.CreateTempSubdirectory("sep-kv");
        var prior = Environment.GetEnvironmentVariable("SMOOTH_HOME");
        Environment.SetEnvironmentVariable("SMOOTH_HOME", tmp.FullName);
        try
        {
            var d = new DefaultHostDelegate();
            Assert.Null(await d.KvGetAsync("kvtest", "missing"));
            await d.KvSetAsync("kvtest", "k", new JsonObject { ["n"] = 1 });
            var got = await d.KvGetAsync("kvtest", "k");
            Assert.Equal("{\"n\":1}", got!.ToJsonString());
        }
        finally
        {
            Environment.SetEnvironmentVariable("SMOOTH_HOME", prior);
            tmp.Delete(recursive: true);
        }
    }

    // ---- empty host: the zero-behavior-change default ----

    [Fact]
    public async Task EmptyHostHookIsPassthrough()
    {
        var host = ExtensionHost.Empty();
        Assert.True(host.IsEmpty);
        AssertProceed(await host.RunHookAsync(HookType.ToolCall, new JsonObject { ["tool"] = "x" }), new JsonObject { ["tool"] = "x" });
        Assert.Equal("prompt", await host.BeforeAgentStartAsync("prompt"));
        Assert.Empty(host.Tools());
        host.DispatchEvent("turn_start", new JsonObject()); // no-op, must not throw
        Assert.Empty(host.Commands());
        Assert.Empty(host.Shortcuts());
    }

    [Fact]
    public void BackoffScheduleExhaustsAfterThree()
    {
        Assert.Equal(TimeSpan.FromSeconds(1), ExtensionProcess.BackoffFor(0));
        Assert.Equal(TimeSpan.FromSeconds(5), ExtensionProcess.BackoffFor(1));
        Assert.Equal(TimeSpan.FromSeconds(25), ExtensionProcess.BackoffFor(2));
        Assert.Null(ExtensionProcess.BackoffFor(3));
    }
}
