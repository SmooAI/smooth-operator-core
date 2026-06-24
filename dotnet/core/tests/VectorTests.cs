using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Phase-1 parity tests: embedding-backed vector knowledge. <see cref="VectorKnowledgeBase"/>
/// satisfies the same <see cref="IKnowledgeBase"/> interface as the lexical store, so an agent
/// accepts either. Mirrors the sibling engines' vector tests.
/// </summary>
public class VectorTests
{
    [Fact]
    public void HashEmbedder_IsDeterministic_AndL2Normalized()
    {
        var embedder = new HashEmbedder(128);
        var a = embedder.Embed("the quick brown fox");
        var b = embedder.Embed("the quick brown fox");

        Assert.Equal(a, b); // deterministic — same text, same vector

        var norm = Math.Sqrt(a.Sum(v => v * v));
        Assert.True(Math.Abs(norm - 1.0) < 1e-9, $"expected unit norm, got {norm}");
    }

    [Fact]
    public void HashEmbedder_EmptyText_IsZeroVector()
    {
        var vec = new HashEmbedder(64).Embed("   ");
        Assert.All(vec, v => Assert.Equal(0.0, v));
    }

    [Fact]
    public void HashEmbedder_RejectsNonPositiveDim()
    {
        Assert.Throws<ArgumentOutOfRangeException>(() => new HashEmbedder(0));
    }

    [Fact]
    public async Task VectorKnowledge_RetrievesTokenOverlapNeighborFirst()
    {
        var kb = new VectorKnowledgeBase();
        await kb.IngestAsync(new KnowledgeDocument("returns", "Our return window is 17 days from delivery.", "policy.md"));
        await kb.IngestAsync(new KnowledgeDocument("hours", "Support hours are 9am to 5pm Central.", "hours.md"));

        var hits = await kb.QueryAsync("how long is the return window", limit: 4);

        Assert.NotEmpty(hits);
        Assert.Equal("returns", hits[0].DocumentId);
        Assert.True(hits[0].Score > 0);
    }

    [Fact]
    public async Task VectorKnowledge_IngestDedupesById()
    {
        var kb = new VectorKnowledgeBase();
        await kb.IngestAsync(new KnowledgeDocument("d", "first version about cats", "src"));
        await kb.IngestAsync(new KnowledgeDocument("d", "second version about cats", "src"));

        var hits = await kb.QueryAsync("cats", limit: 10);

        Assert.Single(hits);
        Assert.Contains("second version", hits[0].Chunk);
    }

    [Fact]
    public async Task VectorKnowledge_EmptyOrNonPositiveLimit_ReturnsEmpty()
    {
        var kb = new VectorKnowledgeBase();
        await kb.IngestAsync(new KnowledgeDocument("d", "content", "src"));

        Assert.Empty(await kb.QueryAsync("content", limit: 0));
        Assert.Empty(await new VectorKnowledgeBase().QueryAsync("anything", limit: 4));
    }

    [Fact]
    public async Task VectorKnowledge_DropsInAsAgentKnowledge()
    {
        var kb = new VectorKnowledgeBase();
        await kb.IngestAsync(new KnowledgeDocument("returns", "The return window is 17 days.", "policy.md"));

        var mock = new MockLlmProvider().PushText("It's 17 days.");
        var agent = new SmoothAgent(mock, new AgentOptions { Knowledge = kb });

        await agent.RunAsync("How long is the return window?");

        // The vector-retrieved chunk reached the model as grounding context, with its source.
        Assert.Contains(mock.Calls[0], m => m.Text.Contains("17 days") && m.Text.Contains("policy.md"));
    }
}
