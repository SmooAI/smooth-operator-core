using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Phase-3 parity tests: checkpoint store operations, checkpointing during a run, and
/// resume-across-restart. Mirrors the Rust engine's checkpoint/<c>resume_or_new</c>.
/// </summary>
public class CheckpointTests
{
    private static Checkpoint MakeCheckpoint(string threadId, int iteration) =>
        new(Guid.NewGuid().ToString("n"), threadId, new List<ChatMessage> { new(ChatRole.User, $"m{iteration}") }, iteration, DateTimeOffset.UtcNow);

    [Fact]
    public async Task Store_Save_LoadLatest_List_Prune()
    {
        var store = new InMemoryCheckpointStore();
        await store.SaveAsync(MakeCheckpoint("t1", 1));
        await store.SaveAsync(MakeCheckpoint("t1", 2));
        await store.SaveAsync(MakeCheckpoint("t2", 1));

        var latest = await store.LoadLatestAsync("t1");
        Assert.NotNull(latest);
        Assert.Equal(2, latest!.Iteration);

        Assert.Equal(2, (await store.ListAsync("t1")).Count);

        var removed = await store.PruneAsync("t1", keep: 1);
        Assert.Equal(1, removed);
        Assert.Single(await store.ListAsync("t1"));
        Assert.Single(await store.ListAsync("t2")); // other threads untouched
    }

    [Fact]
    public async Task Agent_WritesCheckpoint_AfterToolCall()
    {
        var store = new InMemoryCheckpointStore();
        var add = AIFunctionFactory.Create((int a, int b) => a + b, "add", "adds");
        var mock = new MockLlmProvider()
            .PushToolCall("c1", "add", new Dictionary<string, object?> { ["a"] = 2, ["b"] = 3 })
            .PushText("5");
        var options = new AgentOptions { CheckpointStore = store, Checkpoint = CheckpointStrategy.AfterToolCall };
        options.Tools.Add(add);
        var agent = new SmoothAgent(mock, options);
        var thread = agent.GetNewThread();

        await agent.RunAsync("add 2 and 3", thread);

        Assert.NotEmpty(await store.ListAsync(thread.Id));
    }

    [Fact]
    public async Task NoCheckpoint_WhenStrategyNever()
    {
        var store = new InMemoryCheckpointStore();
        var mock = new MockLlmProvider().PushText("hi");
        var agent = new SmoothAgent(mock, new AgentOptions { CheckpointStore = store, Checkpoint = CheckpointStrategy.Never });
        var thread = agent.GetNewThread();

        await agent.RunAsync("hello", thread);

        Assert.Empty(await store.ListAsync(thread.Id));
    }

    [Fact]
    public async Task Resume_RestoresConversation_AcrossRestart()
    {
        var store = new InMemoryCheckpointStore();

        // "Process 1": run turn 1, checkpointing each iteration.
        var mock1 = new MockLlmProvider().PushText("Nice to meet you, Brent.");
        var agent1 = new SmoothAgent(mock1, new AgentOptions { CheckpointStore = store, Checkpoint = CheckpointStrategy.AfterEachIteration });
        var thread1 = agent1.GetNewThread();
        await agent1.RunAsync("My name is Brent.", thread1);
        var threadId = thread1.Id;

        // "Process 2": a brand-new agent resumes the thread from the store, then runs turn 2.
        var mock2 = new MockLlmProvider().PushText("Your name is Brent.");
        var agent2 = new SmoothAgent(mock2, new AgentOptions { CheckpointStore = store, Checkpoint = CheckpointStrategy.AfterEachIteration });
        var resumed = await agent2.ResumeThreadAsync(threadId);
        await agent2.RunAsync("What's my name?", resumed);

        // The resumed turn saw turn 1's user + assistant messages.
        Assert.Contains(mock2.Calls[0], m => m.Text.Contains("My name is Brent."));
        Assert.Contains(mock2.Calls[0], m => m.Text.Contains("Nice to meet you"));
    }
}
