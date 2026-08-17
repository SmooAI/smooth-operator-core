using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Parity tests for the durable-execution seam (ADR-030), mirroring the Rust reference's
/// <c>executor.rs</c> and <c>activities.rs</c> test modules one-for-one so the correspondence is
/// visible. No network: everything runs on <see cref="MockLlmProvider"/>.
/// </summary>
public class AgentExecutorTests
{
    private static AIFunction Echo() => AIFunctionFactory.Create(
        (string text) => text,
        "echo",
        "Echoes input back");

    private static List<ChatMessage> SeedConversation(string user) => new()
    {
        new ChatMessage(ChatRole.System, "You are a test agent"),
        new ChatMessage(ChatRole.User, user),
    };

    // ── executor (Rust: executor.rs) ─────────────────────────────────────────────────────────

    /// <summary>Rust: <c>in_process_executor_matches_agent_run</c>. The in-process executor drives
    /// the loop identically to calling <c>RunAsync</c>: one model call, the scripted content.</summary>
    [Fact]
    public async Task InProcessExecutor_MatchesRunAsync()
    {
        var mock = new MockLlmProvider().PushText("the answer is 42");
        var agent = new SmoothAgent(mock, new AgentOptions());

        var result = await new InProcessExecutor().ExecuteAsync(agent, "what is the answer?");

        Assert.Equal("the answer is 42", result.Text);
        Assert.Equal(1, mock.CallCount);
        Assert.Contains(mock.Calls[0], m => m.Text.Contains("what is the answer?"));
    }

    /// <summary>Rust: <c>in_process_executor_streaming_emits_events_and_returns_conversation</c>.
    /// The streaming entry point surfaces updates and ends on the same final text.</summary>
    [Fact]
    public async Task InProcessExecutor_Streaming_EmitsUpdates_AndSameFinalResult()
    {
        var mock = new MockLlmProvider().PushText("streamed reply");
        var agent = new SmoothAgent(mock, new AgentOptions());

        var updates = new List<ChatResponseUpdate>();
        await foreach (var update in new InProcessExecutor().ExecuteStreamingAsync(agent, "stream please"))
        {
            updates.Add(update);
        }

        Assert.NotEmpty(updates);
        Assert.Contains("streamed reply", string.Concat(updates.Select(u => u.Text)));
    }

    /// <summary>Rust: <c>executor_is_object_safe</c>. The seam is usable through the interface,
    /// which is what lets a consumer hold an <see cref="IAgentExecutor"/> and swap backends.</summary>
    [Fact]
    public async Task Executor_IsUsableThroughInterface()
    {
        var mock = new MockLlmProvider().PushText("dyn ok");
        var agent = new SmoothAgent(mock, new AgentOptions());

        IAgentExecutor executor = new InProcessExecutor();
        var result = await executor.ExecuteAsync(agent, "via the interface");

        Assert.Equal("dyn ok", result.Text);
    }

    // ── drive_turn (Rust: activities.rs) ─────────────────────────────────────────────────────

    /// <summary>Rust: <c>drive_turn_text_reply_stops_after_one_model_call</c>.</summary>
    [Fact]
    public async Task DriveTurn_TextReply_StopsAfterOneModelCall()
    {
        var mock = new MockLlmProvider().PushText("the answer is 42");
        var activities = new InProcessActivities(mock);

        var messages = SeedConversation("what is the answer?");
        await AgentTurn.DriveTurnAsync(activities, messages, Array.Empty<AITool>(), new TurnPolicy());

        Assert.Equal(1, mock.CallCount);
        Assert.Equal("the answer is 42", messages.Last(m => m.Role == ChatRole.Assistant).Text);
    }

    /// <summary>Rust: <c>drive_turn_runs_tool_then_finishes</c>. The tool runs, its result lands as
    /// a Tool-role message paired to the call by id + name, and the loop goes back to the model.</summary>
    [Fact]
    public async Task DriveTurn_RunsTool_AppendsPairedResult_AndLoopsBack()
    {
        var mock = new MockLlmProvider()
            .PushToolCall("call-1", "echo", new Dictionary<string, object?> { ["text"] = "hello tools" })
            .PushText("done");
        var activities = new InProcessActivities(mock, new[] { Echo() });

        var messages = SeedConversation("use the echo tool");
        await AgentTurn.DriveTurnAsync(activities, messages, Array.Empty<AITool>(), new TurnPolicy());

        Assert.Equal(2, mock.CallCount);
        var toolMessages = messages.Where(m => m.Role == ChatRole.Tool).ToList();
        var toolMessage = Assert.Single(toolMessages);
        Assert.Equal("echo", toolMessage.AuthorName);
        var resultContent = Assert.Single(toolMessage.Contents.OfType<FunctionResultContent>());
        Assert.Equal("call-1", resultContent.CallId);
        Assert.Contains("hello tools", resultContent.Result?.ToString());
        Assert.Equal("done", messages.Last(m => m.Role == ChatRole.Assistant).Text);
    }

    /// <summary>Rust: the <c>max_iterations</c> bound — a model that never stops asking for tools is
    /// cut off at the policy's cap, and the turn returns what it has rather than throwing.</summary>
    [Fact]
    public async Task DriveTurn_MaxIterations_BoundsTheLoop()
    {
        var mock = new MockLlmProvider();
        for (var i = 0; i < 10; i++)
        {
            mock.PushToolCall($"call-{i}", "echo", new Dictionary<string, object?> { ["text"] = "again" });
        }
        var activities = new InProcessActivities(mock, new[] { Echo() });

        var messages = SeedConversation("loop forever");
        await AgentTurn.DriveTurnAsync(activities, messages, Array.Empty<AITool>(), new TurnPolicy { MaxIterations = 3 });

        Assert.Equal(3, mock.CallCount);
        Assert.Equal(3, messages.Count(m => m.Role == ChatRole.Tool));
    }
}
