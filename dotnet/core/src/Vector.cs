using System.Text.RegularExpressions;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// Turns text into a dense vector. Pluggable so a production adapter can call the gateway's
/// <c>/embeddings</c> endpoint, while the bundled <see cref="HashEmbedder"/> stays deterministic
/// and offline for tests. Mirrors the Rust engine's <c>Embedder</c> and the sibling engines'
/// <c>Embedder</c> seam.
/// </summary>
public interface IEmbedder
{
    IReadOnlyList<double> Embed(string text);
}

/// <summary>
/// A deterministic, offline feature-hashing embedder: hashes each token (FNV-1a) into one of
/// <c>dim</c> signed buckets and L2-normalizes. No learned semantics, but a real vector with
/// cosine geometry — documents that share tokens land near each other. The C# analog of the
/// sibling engines' <c>HashEmbedder</c>.
/// </summary>
public sealed class HashEmbedder : IEmbedder
{
    private static readonly Regex TokenPattern = new("[a-z0-9]+", RegexOptions.Compiled);

    private readonly int _dim;

    public HashEmbedder(int dim = 256)
    {
        if (dim <= 0)
        {
            throw new ArgumentOutOfRangeException(nameof(dim), "dim must be positive");
        }
        _dim = dim;
    }

    /// <summary>FNV-1a (32-bit) over the token's bytes — a small, fast, non-cryptographic hash.</summary>
    public static uint HashToken(string token)
    {
        uint h = 0x811c9dc5;
        foreach (var ch in token)
        {
            h ^= (byte)ch;
            h *= 0x01000193;
        }
        return h;
    }

    public IReadOnlyList<double> Embed(string text)
    {
        var vec = new double[_dim];
        foreach (Match match in TokenPattern.Matches(text.ToLowerInvariant()))
        {
            var h = HashToken(match.Value);
            var bucket = (int)(h % (uint)_dim);
            var sign = (h >> 31) == 1 ? -1.0 : 1.0;
            vec[bucket] += sign;
        }

        var norm = Math.Sqrt(vec.Sum(v => v * v));
        if (norm > 0)
        {
            for (var i = 0; i < vec.Length; i++)
            {
                vec[i] /= norm;
            }
        }
        return vec;
    }
}

/// <summary>
/// An embedding-backed <see cref="IKnowledgeBase"/> with cosine-similarity retrieval. Satisfies
/// the same interface as <see cref="InMemoryKnowledgeBase"/>, so an agent accepts either: lexical
/// overlap or true vector semantics, swapped behind <see cref="AgentOptions.Knowledge"/>. Mirrors
/// the sibling engines' <c>VectorKnowledge</c>.
/// </summary>
public sealed class VectorKnowledgeBase : IKnowledgeBase
{
    private readonly IEmbedder _embedder;
    private readonly List<Entry> _docs = new();

    public VectorKnowledgeBase(IEmbedder? embedder = null)
    {
        _embedder = embedder ?? new HashEmbedder();
    }

    public Task IngestAsync(KnowledgeDocument document, CancellationToken cancellationToken = default)
    {
        _docs.RemoveAll(d => d.Document.Id == document.Id);
        _docs.Add(new Entry(document, _embedder.Embed(document.Content)));
        return Task.CompletedTask;
    }

    public Task<IReadOnlyList<KnowledgeResult>> QueryAsync(string query, int limit, CancellationToken cancellationToken = default)
    {
        if (limit <= 0 || _docs.Count == 0)
        {
            return Task.FromResult<IReadOnlyList<KnowledgeResult>>(Array.Empty<KnowledgeResult>());
        }

        var q = _embedder.Embed(query);
        IReadOnlyList<KnowledgeResult> hits = _docs
            .Select(e => new KnowledgeResult(e.Document.Id, e.Document.Content, Cosine(q, e.Embedding), e.Document.Source))
            .Where(r => r.Score > 0)
            .OrderByDescending(r => r.Score)
            .Take(limit)
            .ToList();
        return Task.FromResult(hits);
    }

    private static double Cosine(IReadOnlyList<double> a, IReadOnlyList<double> b)
    {
        // Both vectors are L2-normalized by the embedder, so the dot product is the cosine.
        var sum = 0.0;
        var n = Math.Min(a.Count, b.Count);
        for (var i = 0; i < n; i++)
        {
            sum += a[i] * b[i];
        }
        return sum;
    }

    private readonly record struct Entry(KnowledgeDocument Document, IReadOnlyList<double> Embedding);
}
