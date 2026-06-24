using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Reranker parity with the Rust engine's <c>rerank</c> module: the lexical reranker promotes the
/// best lexical match, the noop reranker is the identity, both truncate to top-K, a no-overlap
/// query preserves order, and the opt-in helper reorders only when given a reranker.
/// </summary>
public class RerankTests
{
    private static KnowledgeResult Result(string id, string chunk, double score) => new(id, chunk, score, $"{id}.md");

    [Fact]
    public async Task LexicalReranker_PromotesBestLexicalMatch()
    {
        const string query = "return policy refund window";
        // Upstream order puts a weak match first and the strong match third.
        var candidates = new[]
        {
            Result("shipping", "Standard shipping takes 5 to 7 business days.", 0.9),
            Result("warranty", "Warranty claims must be filed within one year.", 0.8),
            Result("returns", "Our return policy: refunds are issued within the 30 day return window.", 0.7),
        };

        var reranked = await new LexicalReranker().RerankAsync(query, candidates, 3);

        Assert.Equal("returns", reranked[0].DocumentId);
    }

    [Fact]
    public async Task NoopReranker_IsIdentity()
    {
        const string query = "anything at all";
        var candidates = new[]
        {
            Result("a", "first chunk about returns and refunds", 0.9),
            Result("b", "second chunk about shipping", 0.8),
            Result("c", "third chunk about returns refund window", 0.7),
        };

        var reranked = await new NoopReranker().RerankAsync(query, candidates, 3);

        Assert.Equal(new[] { "a", "b", "c" }, reranked.Select(r => r.DocumentId));
    }

    [Fact]
    public async Task NoopReranker_TruncatesToTopK()
    {
        var candidates = new[] { Result("a", "alpha", 0.9), Result("b", "beta", 0.8), Result("c", "gamma", 0.7) };

        var reranked = await new NoopReranker().RerankAsync("q", candidates, 2);

        Assert.Equal(2, reranked.Count);
        Assert.Equal("a", reranked[0].DocumentId);
        Assert.Equal("b", reranked[1].DocumentId);
    }

    [Fact]
    public async Task LexicalReranker_TruncatesAfterReorder()
    {
        var candidates = new[]
        {
            Result("shipping", "shipping times and delivery", 0.9),
            Result("returns", "refund and returns policy details", 0.8),
            Result("misc", "unrelated content here", 0.7),
        };

        var reranked = await new LexicalReranker().RerankAsync("refund returns", candidates, 1);

        Assert.Single(reranked);
        Assert.Equal("returns", reranked[0].DocumentId);
    }

    [Fact]
    public async Task LexicalReranker_NoOverlap_PreservesOrder()
    {
        // No query term appears in any chunk → all zero scores → stable sort keeps upstream order.
        var candidates = new[] { Result("a", "shipping and delivery", 0.9), Result("b", "returns and refunds", 0.8) };

        var reranked = await new LexicalReranker().RerankAsync("quantum entanglement physics", candidates, 2);

        Assert.Equal("a", reranked[0].DocumentId);
        Assert.Equal("b", reranked[1].DocumentId);
    }

    [Fact]
    public async Task ApplyOptional_Null_TruncatesOnly()
    {
        var candidates = new[] { Result("a", "shipping", 0.9), Result("returns", "refund refund refund window", 0.8) };

        var output = await Rerankers.ApplyOptionalAsync(null, "refund", candidates, 2);

        Assert.Equal("a", output[0].DocumentId); // order preserved, no reorder
    }

    [Fact]
    public async Task ApplyOptional_WithReranker_Reorders()
    {
        var candidates = new[]
        {
            Result("a", "shipping and delivery times", 0.9),
            Result("returns", "refund window details and policy", 0.8),
        };

        var output = await Rerankers.ApplyOptionalAsync(new LexicalReranker(), "refund window", candidates, 2);

        Assert.Equal("returns", output[0].DocumentId);
    }
}
