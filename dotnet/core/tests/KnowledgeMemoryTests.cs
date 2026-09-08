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

    // ── cross-language recall contract (th-ffaeae) ───────────────────────────
    //
    // The block below is reproduced byte-for-byte by the Rust reference and the Python,
    // Go and TypeScript siblings. These tests mirror
    // rust/smooth-operator-core/src/memory.rs so a drift shows up here, not months later
    // when someone tries to write a shared conformance scenario.

    [Fact]
    public void RecallBlock_IsPinnedAcrossLanguages()
    {
        var block = MemoryRecall.RenderRecallBlock(
            [new MemoryEntry("m1", "brent prefers execution over questions", MemoryType.User, Relevance: 0.5)]);
        Assert.Equal("[Recalled memories]\n- (User, relevance=0.50): brent prefers execution over questions\n", block);
    }

    /// <summary>A Project/Reference memory names something in a moving codebase, so the model is
    /// told to verify it. A User/Feedback one describes the person and does not go stale —
    /// emitting the note there would train the model to skip it.</summary>
    [Fact]
    public void RecallBlock_AddsFreshnessNote_OnlyWhenTimeSensitive()
    {
        var block = MemoryRecall.RenderRecallBlock(
            [new MemoryEntry("m1", "the retry lives in fetch.rs", MemoryType.Project, Relevance: 1.0)]);
        Assert.Equal(
            $"{MemoryRecall.RecallHeader}\n{MemoryRecall.RecallFreshnessNote}\n- (Project, relevance=1.00): the retry lives in fetch.rs\n",
            block);

        var durable = MemoryRecall.RenderRecallBlock(
            [new MemoryEntry("m2", "prefers dark mode", MemoryType.User, Relevance: 1.0)]);
        Assert.DoesNotContain("Note:", durable, StringComparison.Ordinal);
    }

    /// <summary>A bare header would spend context telling the model it remembered nothing.</summary>
    [Fact]
    public void EmptyRecall_RendersNoBlock() => Assert.Null(MemoryRecall.RenderRecallBlock([]));

    /// <summary>Normalised to 0-1 so it is comparable between entries AND between languages —
    /// a raw overlap count is neither, and it is rendered into the prompt.</summary>
    [Fact]
    public void Relevance_IsAFractionOfQueryTokens()
    {
        Assert.Equal(0.5, MemoryRecall.RelevanceScore("watchlist on marvin today", "the watchlist lives on smoo-hub"));
        Assert.Equal(0, MemoryRecall.RelevanceScore("", "anything"));
    }

    /// <summary>Scoring used to split on whitespace only, so "do you remember my name?" scored 0
    /// against "the user's name is Dana" — the trailing '?' made <c>name?</c> fail — and the
    /// memory was silently never recalled. C# additionally dropped every token of length &lt;= 2,
    /// so "my" could never contribute at all.</summary>
    [Fact]
    public void Punctuation_DoesNotDefeatAMatch()
    {
        Assert.True(MemoryRecall.RelevanceScore("do you remember my name?", "The user's name is Dana.") > 0);
        Assert.Equal(1.0, MemoryRecall.RelevanceScore("watchlist!", "the watchlist lives here"));
    }

    [Fact]
    public async Task Recall_PopulatesRelevanceAndType()
    {
        var memory = new InMemoryAgentMemory();
        await memory.StoreAsync(new MemoryEntry("m1", "the watchlist lives on smoo-hub", MemoryType.Project));
        var hits = await memory.RecallAsync("watchlist on marvin today", MemoryRecall.MemoryTopK);
        Assert.Single(hits);
        Assert.Equal(0.5, hits[0].Relevance);
        Assert.Equal(MemoryType.Project, hits[0].Type);
    }
}

