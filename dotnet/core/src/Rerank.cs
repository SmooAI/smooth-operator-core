namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// Reorder retrieval candidates by query relevance, returning at most <c>topK</c>,
/// best first. The C# analog of the Rust engine's <c>Reranker</c> trait — the opt-in
/// post-retrieval reorder stage. Implementations must be total: an empty candidate set yields an
/// empty result, and <c>topK == 0</c> yields an empty result.
/// </summary>
public interface IReranker
{
    Task<IReadOnlyList<KnowledgeResult>> RerankAsync(
        string query,
        IReadOnlyList<KnowledgeResult> candidates,
        int topK,
        CancellationToken cancellationToken = default);
}

/// <summary>
/// Identity reranker — the behavior-preserving default. Leaves candidate order untouched and
/// truncates to <c>topK</c>, so wiring it in is a no-op versus not reranking at all (which is what
/// makes the rerank stage opt-in). Mirrors the Rust <c>NoopReranker</c>.
/// </summary>
public sealed class NoopReranker : IReranker
{
    public Task<IReadOnlyList<KnowledgeResult>> RerankAsync(string query, IReadOnlyList<KnowledgeResult> candidates, int topK, CancellationToken cancellationToken = default)
    {
        IReadOnlyList<KnowledgeResult> result = topK <= 0 ? Array.Empty<KnowledgeResult>() : candidates.Take(topK).ToArray();
        return Task.FromResult(result);
    }
}

/// <summary>
/// Deterministic, network-free lexical reranker. Scores each candidate by how much of the query's
/// vocabulary its chunk contains — a small BM25-ish lexical signal (term-frequency saturated and
/// length-normalized), computed entirely offline. No embeddings, no network, no cost, fully
/// reproducible — the rerank analog of the <c>DeterministicEmbedder</c>'s role on the dense
/// path. Ties (and zero-overlap candidates) keep their upstream order (stable sort), so a no-signal
/// query degrades to the upstream ranking rather than shuffling. Mirrors the Rust
/// <c>LexicalReranker</c> (score = Σ tf_saturated(q) / (1 + ln(1 + chunk_len))).
/// </summary>
public sealed class LexicalReranker : IReranker
{
    private readonly double _k1;

    public LexicalReranker(double k1 = 1.2) => _k1 = k1;

    public Task<IReadOnlyList<KnowledgeResult>> RerankAsync(string query, IReadOnlyList<KnowledgeResult> candidates, int topK, CancellationToken cancellationToken = default)
    {
        if (candidates.Count == 0 || topK <= 0)
        {
            return Task.FromResult<IReadOnlyList<KnowledgeResult>>(Array.Empty<KnowledgeResult>());
        }

        var queryTerms = new HashSet<string>(Tokenize(query));

        // Pair each candidate with its lexical score and stable-sort by score descending.
        // OrderByDescending is stable in .NET, so equal-scored (and zero-overlap) candidates
        // retain their upstream order.
        IReadOnlyList<KnowledgeResult> reranked = candidates
            .Select((candidate, index) => (candidate, index, score: Score(queryTerms, candidate.Chunk)))
            .OrderByDescending(x => x.score)
            .Take(topK)
            .Select(x => x.candidate)
            .ToArray();

        return Task.FromResult(reranked);
    }

    /// <summary>Lowercase alphanumeric tokenization, matching the dense embedder's split.</summary>
    private static IEnumerable<string> Tokenize(string text) =>
        text.ToLowerInvariant().Split(c => !char.IsLetterOrDigit(c));

    private double Score(HashSet<string> queryTerms, string chunk)
    {
        var chunkTokens = Tokenize(chunk).ToArray();
        if (chunkTokens.Length == 0)
        {
            return 0.0;
        }
        var lengthPenalty = 1.0 + Math.Log(1.0 + chunkTokens.Length);

        var score = 0.0;
        foreach (var term in queryTerms)
        {
            double count = chunkTokens.Count(t => t == term);
            if (count > 0.0)
            {
                var tfSaturated = count / (count + _k1);
                score += tfSaturated / lengthPenalty;
            }
        }
        return score;
    }
}

/// <summary>Rerank helpers, including the opt-in <see cref="ApplyOptionalAsync"/>.</summary>
public static class Rerankers
{
    /// <summary>
    /// Apply an optional reranker to a freshly-retrieved candidate set. Pass a reranker to reorder
    /// the top-K, or <c>null</c> to keep the upstream order (merely truncated to <c>topK</c>).
    /// Centralizing the null handling here keeps the call sites minimal and makes "reranking is off
    /// by default" a single obvious branch. Mirrors the Rust <c>apply_optional_rerank</c>.
    /// </summary>
    public static async Task<IReadOnlyList<KnowledgeResult>> ApplyOptionalAsync(
        IReranker? reranker,
        string query,
        IReadOnlyList<KnowledgeResult> candidates,
        int topK,
        CancellationToken cancellationToken = default)
    {
        if (reranker is not null)
        {
            return await reranker.RerankAsync(query, candidates, topK, cancellationToken).ConfigureAwait(false);
        }
        return topK <= 0 ? Array.Empty<KnowledgeResult>() : candidates.Take(topK).ToArray();
    }
}

internal static class RerankStringExtensions
{
    /// <summary>Split on a char predicate, dropping empty segments — the analog of Rust's split+filter.</summary>
    public static IEnumerable<string> Split(this string text, Func<char, bool> isSeparator)
    {
        var start = 0;
        for (var i = 0; i < text.Length; i++)
        {
            if (isSeparator(text[i]))
            {
                if (i > start)
                {
                    yield return text[start..i];
                }
                start = i + 1;
            }
        }
        if (text.Length > start)
        {
            yield return text[start..];
        }
    }
}
