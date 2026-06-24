using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Phase-1 parity tests: multi-turn conversation continuity (a <see cref="SmoothAgentThread"/>
/// carries history across turns) and context-window compaction.
/// </summary>
public class ConversationTests
{
    [Fact]
    public async Task Thread_CarriesHistoryAcrossTurns()
    {
        var mock = new MockLlmProvider()
            .PushText("Nice to meet you, Brent.")
            .PushText("Your name is Brent.");
        var agent = new SmoothAgent(mock, new AgentOptions { Instructions = "be helpful" });
        var thread = agent.GetNewThread();

        await agent.RunAsync("My name is Brent.", thread);
        var second = await agent.RunAsync("What's my name?", thread);

        Assert.Equal("Your name is Brent.", second.Text);
        // The second model call saw the first turn's user + assistant messages.
        var secondCall = mock.Calls[1];
        Assert.Contains(secondCall, m => m.Role == ChatRole.User && m.Text.Contains("My name is Brent."));
        Assert.Contains(secondCall, m => m.Role == ChatRole.Assistant && m.Text.Contains("Nice to meet you"));
        // The thread accumulated all four messages (two user, two assistant).
        Assert.Equal(4, thread.Count);
    }

    [Fact]
    public async Task StatelessRun_DoesNotLeakIntoNextRun()
    {
        var mock = new MockLlmProvider().PushText("first").PushText("second");
        var agent = new SmoothAgent(mock, new AgentOptions());

        await agent.RunAsync("turn one");
        await agent.RunAsync("turn two");

        // No thread → the second call must NOT contain the first turn's content.
        Assert.DoesNotContain(mock.Calls[1], m => m.Text.Contains("turn one"));
    }

    [Fact]
    public async Task Compaction_TrimsOldMessages_PreservingSystemAndLatestUser()
    {
        var mock = new MockLlmProvider().PushText("ok");
        var options = new AgentOptions
        {
            Instructions = "SYSTEM PROMPT",
            MaxContextTokens = 60,
            Compaction = CompactionStrategy.SlidingWindow,
        };
        var agent = new SmoothAgent(mock, options);

        // Pre-load a long history that blows past the 60-token budget.
        var thread = agent.GetNewThread();
        var filler = new string('x', 80); // ~24 estimated tokens each
        for (var i = 0; i < 6; i++)
        {
            thread.Add(new ChatMessage(ChatRole.User, $"old user {i} {filler}"));
            thread.Add(new ChatMessage(ChatRole.Assistant, $"old assistant {i} {filler}"));
        }

        await agent.RunAsync("brand new question", thread);

        var sentToModel = mock.Calls[0];
        // System prompt preserved at the front.
        Assert.Equal(ChatRole.System, sentToModel[0].Role);
        Assert.Contains("SYSTEM PROMPT", sentToModel[0].Text);
        // The live user message preserved at the end.
        Assert.Contains("brand new question", sentToModel[^1].Text);
        // It was actually compacted — far fewer than the 1 system + 12 history + 1 user = 14 messages.
        Assert.True(sentToModel.Count < 14, $"expected compaction, got {sentToModel.Count} messages");
        // And it now fits the budget.
        Assert.True(Compactor.EstimateTokens(sentToModel) <= 60);
    }

    [Fact]
    public void Compactor_None_IsNoOp_EvenOverBudget()
    {
        var messages = new List<ChatMessage>
        {
            new(ChatRole.System, new string('a', 400)),
            new(ChatRole.User, new string('b', 400)),
            new(ChatRole.Assistant, new string('c', 400)),
        };
        var result = Compactor.Compact(messages, CompactionStrategy.None, maxTokens: 10);

        Assert.False(result.Compacted);
        Assert.Equal(3, messages.Count);
    }
}
