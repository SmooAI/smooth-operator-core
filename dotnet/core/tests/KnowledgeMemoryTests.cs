using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Phase-2 parity tests: pluggable knowledge + memory, retrieved and injected as pre-turn
/// grounding context (RAG). Mirrors the Rust core's knowledge/memory injection.
/// </summary>
public class KnowledgeMemoryTests
{
    [Fact]
    public async Task Knowledge_Query_RanksLexicalMatchesFirst()
    {
        var kb = new InMemoryKnowledgeBase();
        await kb.IngestAsync(new KnowledgeDocument("returns", "Our return window is 17 days from delivery.", "policy.md"));
        await kb.IngestAsync(new KnowledgeDocument("hours", "Support hours are 9am to 5pm Central.", "policy.md"));

        var hits = await kb.QueryAsync("how long is the return window?", limit: 4);

        Assert.NotEmpty(hits);
        Assert.Equal("returns", hits[0].DocumentId);
        Assert.Contains("17 days", hits[0].Chunk);
    }

    [Fact]
    public async Task Agent_InjectsRetrievedKnowledge_AsGroundingContext()
    {
        var kb = new InMemoryKnowledgeBase();
        await kb.IngestAsync(new KnowledgeDocument("returns", "The return window is 17 days.", "policy.md"));

        var mock = new MockLlmProvider().PushText("It's 17 days.");
        var options = new AgentOptions { Instructions = "be helpful", Knowledge = kb };
        var agent = new SmoothAgent(mock, options);

        await agent.RunAsync("How long is the return window?");

        // The retrieved chunk reached the model as grounding context, with its source.
        var sent = mock.Calls[0];
        Assert.Contains(sent, m => m.Text.Contains("17 days") && m.Text.Contains("policy.md"));
    }

    [Fact]
    public async Task Agent_WithNoKnowledgeHit_InjectsNoContext()
    {
        var kb = new InMemoryKnowledgeBase();
        await kb.IngestAsync(new KnowledgeDocument("hours", "Support hours are 9 to 5.", "policy.md"));

        var mock = new MockLlmProvider().PushText("I don't have that.");
        var agent = new SmoothAgent(mock, new AgentOptions { Knowledge = kb });

        await agent.RunAsync("What is the meaning of life?");

        // No lexical overlap → no injected knowledge block.
        Assert.DoesNotContain(mock.Calls[0], m => m.Text.Contains("Relevant knowledge"));
    }

    [Fact]
    public async Task Memory_Store_Recall_Forget()
    {
        var memory = new InMemoryAgentMemory();
        await memory.StoreAsync(new MemoryEntry("u1", "The user's name is Brent.", MemoryType.User));
        await memory.StoreAsync(new MemoryEntry("u2", "The user prefers concise answers.", MemoryType.User));

        var recalled = await memory.RecallAsync("what is the user's name?", limit: 4);
        Assert.Contains(recalled, m => m.Id == "u1");

        await memory.ForgetAsync("u1");
        var afterForget = await memory.RecallAsync("what is the user's name?", limit: 4);
        Assert.DoesNotContain(afterForget, m => m.Id == "u1");
    }

    [Fact]
    public async Task Agent_InjectsRecalledMemory_AsContext()
    {
        var memory = new InMemoryAgentMemory();
        await memory.StoreAsync(new MemoryEntry("u1", "The user's name is Brent.", MemoryType.User));

        var mock = new MockLlmProvider().PushText("Hi Brent!");
        var agent = new SmoothAgent(mock, new AgentOptions { Memory = memory });

        await agent.RunAsync("What is my name?");

        Assert.Contains(mock.Calls[0], m => m.Text.Contains("The user's name is Brent."));
    }
}
