using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Temporal.Tests;

/// <summary>
/// The durable path must refuse exactly what the inline path refuses. <see cref="AgentTurnActivities.ToolInvoke"/>
/// is the seam where that can silently stop being true: it used to resolve the tool by name and call
/// its delegate, so a consumer running <c>PermissionMode = DenyUnmatched</c> with a production
/// <c>DenyPolicy</c> lost the whole gate the moment they swapped <c>InProcessExecutor</c> for
/// <see cref="TemporalExecutor"/>.
///
/// <para><b>Deliberately not in <c>E2ETests</c>.</b> That class is gated on <c>SMOOTH_AGENT_TEMPORAL_E2E</c>,
/// which .NET CI never sets — a security regression proved only there would go unnoticed. An activity
/// is an ordinary method, and the turn seeding is pure, so both are tested directly with no Temporal
/// server and they run on every PR.</para>
/// </summary>
public class DurableGateTests
{
    private static (AgentTurnActivities activities, Func<bool> ran) GatedBashWorker(AgentOptions options)
    {
        var ran = false;
        options.Tools.Add(AIFunctionFactory.Create(
            (string cmd) =>
            {
                _ = cmd;
                ran = true;
                return "ran";
            },
            "bash",
            "run a shell command"));
        var agent = new SmoothAgent(new MockLlmProvider(), options);
        return (new AgentTurnActivities(new EngineHandles(agent)), () => ran);
    }

    /// <summary>Build the activity input the way the workflow does — through the serde boundary, so
    /// the arguments arrive as <c>JsonElement</c>s exactly as they would off the wire. A gate that
    /// only matches pre-serialization arguments would be no gate at all in production.</summary>
    private static ToolInvokeInput BashCall(string command) =>
        new(FunctionCallDto.From(new FunctionCallContent("call-1", "bash", new Dictionary<string, object?> { ["cmd"] = command })));

    [Fact]
    public async Task ToolInvoke_HonorsDenyPolicy_ToolNeverRuns()
    {
        var (activities, ran) = GatedBashWorker(new AgentOptions
        {
            DenyPolicy = DenyPolicy.FromToml("[bash]\ndeny_patterns = [\"aws * --profile prod\"]\n"),
        });

        var output = await activities.ToolInvoke(BashCall("aws s3 rm s3://bucket --profile prod"));

        Assert.False(ran(), "a deny-policy match must never reach the tool on the durable path");
        Assert.True(output.IsError);
        Assert.Contains("denied", output.Content, StringComparison.OrdinalIgnoreCase);
    }

    [Fact]
    public async Task ToolInvoke_HonorsCircuitBreaker_UnderDenyUnmatched()
    {
        // The exact posture a headless durable worker runs in: never ask, deny anything unmatched.
        var (activities, ran) = GatedBashWorker(new AgentOptions { PermissionMode = AutoMode.DenyUnmatched });

        var output = await activities.ToolInvoke(BashCall("rm -rf /"));

        Assert.False(ran(), "a circuit-breaker command must never reach the tool on the durable path");
        Assert.True(output.IsError);
        Assert.Contains("denied", output.Content, StringComparison.OrdinalIgnoreCase);
    }

    [Fact]
    public async Task ToolInvoke_AllowedCall_StillRuns()
    {
        // Not a blanket refusal: the gate has to let ordinary calls through, result unchanged.
        var (activities, ran) = GatedBashWorker(new AgentOptions
        {
            DenyPolicy = DenyPolicy.FromToml("[bash]\ndeny_patterns = [\"aws * --profile prod\"]\n"),
        });

        var output = await activities.ToolInvoke(BashCall("ls -la"));

        Assert.True(ran(), "a call nothing denies must still execute");
        Assert.False(output.IsError);
        Assert.Equal("ran", output.Content);
    }

    [Fact]
    public async Task ToolInvoke_UngatedAgent_RunsToolUnchanged()
    {
        // Additive no-op: with neither a permission mode nor a deny policy configured, the durable
        // path behaves exactly as it did before the gate was wired in.
        var (activities, ran) = GatedBashWorker(new AgentOptions());

        var output = await activities.ToolInvoke(BashCall("rm -rf /"));

        Assert.True(ran(), "gate off → tool executes unchanged");
        Assert.False(output.IsError);
        Assert.Equal("ran", output.Content);
    }

    [Fact]
    public async Task ToolInvoke_UnknownTool_IsReportedAsAToolError()
    {
        var (activities, _) = GatedBashWorker(new AgentOptions());

        var output = await activities.ToolInvoke(
            new ToolInvokeInput(FunctionCallDto.From(new FunctionCallContent("call-1", "nope", new Dictionary<string, object?>()))));

        Assert.True(output.IsError);
        Assert.Contains("unknown tool", output.Content, StringComparison.OrdinalIgnoreCase);
    }

    // ── thread carriage (the durable turn is not amnesiac) ───────────────────────────────────

    [Fact]
    public void SeedConversation_ReplaysHistoryBetweenSystemAndUser()
    {
        var history = new[]
        {
            ChatMessageDto.From(new ChatMessage(ChatRole.User, "my name is Ada")),
            ChatMessageDto.From(new ChatMessage(ChatRole.Assistant, "nice to meet you, Ada")),
        };
        var input = new AgentTurnInput("You are a test agent", "what is my name?", History: history);

        var seeded = input.SeedConversation();

        Assert.Equal(4, seeded.Count);
        Assert.Equal(ChatRole.System, seeded[0].Role);
        Assert.Equal("my name is Ada", seeded[1].Text);
        Assert.Equal("nice to meet you, Ada", seeded[2].Text);
        Assert.Equal(ChatRole.User, seeded[3].Role);
        Assert.Equal("what is my name?", seeded[3].Text);
        // System prompt + the two carried messages precede anything this turn produces.
        Assert.Equal(3, input.CarriedCount);
    }

    [Fact]
    public void SeedConversation_WithoutHistory_IsSystemThenUser()
    {
        var input = new AgentTurnInput("You are a test agent", "hello");

        var seeded = input.SeedConversation();

        Assert.Equal(2, seeded.Count);
        Assert.Equal(ChatRole.System, seeded[0].Role);
        Assert.Equal(ChatRole.User, seeded[1].Role);
        Assert.Equal(1, input.CarriedCount);
    }

    /// <summary>An assistant turn that requested tools, and the tool result paired to it by call id,
    /// have to survive the workflow-result projection — a lossy round trip would replay a
    /// conversation the model rejects on the NEXT durable turn.</summary>
    [Fact]
    public void TurnConversation_RoundTrip_KeepsToolCallsPairedToResults()
    {
        var assistant = new ChatMessage(ChatRole.Assistant, new List<AIContent>
        {
            new FunctionCallContent("call-1", "echo", new Dictionary<string, object?> { ["text"] = "hi" }),
        });
        var tool = new ChatMessage(ChatRole.Tool, new List<AIContent> { new FunctionResultContent("call-1", "hi") })
        {
            AuthorName = "echo",
        };

        var conversation = new TurnConversation(new[] { assistant, tool }.Select(ChatMessageDto.From).ToList());
        var restored = conversation.Messages.Select(m => m.ToChatMessage()).ToList();

        var restoredCall = Assert.Single(restored[0].Contents.OfType<FunctionCallContent>());
        Assert.Equal("call-1", restoredCall.CallId);
        Assert.Equal("echo", restoredCall.Name);
        var restoredResult = Assert.Single(restored[1].Contents.OfType<FunctionResultContent>());
        Assert.Equal("call-1", restoredResult.CallId);
        Assert.Equal("hi", restoredResult.Result?.ToString());
        Assert.Equal("echo", restored[1].AuthorName);
    }
}
