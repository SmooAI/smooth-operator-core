namespace SmooAI.SmoothOperator.Core;

/// <summary>Kind of source a knowledge document came from. Mirrors the Rust <c>DocumentType</c>.</summary>
public enum DocumentType
{
    Code,
    Markdown,
    Config,
    Documentation,
    Conversation,
}

/// <summary>A document ingested into a <see cref="IKnowledgeBase"/>.</summary>
public sealed record KnowledgeDocument(
    string Id,
    string Content,
    string Source,
    DocumentType DocType = DocumentType.Documentation,
    IReadOnlyDictionary<string, string>? Metadata = null);

/// <summary>A retrieval hit: the chunk that matched, its score, and where it came from.</summary>
public sealed record KnowledgeResult(string DocumentId, string Chunk, double Score, string Source);

/// <summary>
/// A pluggable knowledge store the agent retrieves from before answering. Mirrors the Rust
/// engine's <c>KnowledgeBase</c> trait. Production adapters do embeddings + ANN; the bundled
/// <see cref="InMemoryKnowledgeBase"/> does deterministic lexical scoring for tests and small
/// in-process corpora. (Chunking is a later phase — for now a document is its own chunk.)
/// </summary>
public interface IKnowledgeBase
{
    Task IngestAsync(KnowledgeDocument document, CancellationToken cancellationToken = default);

    Task<IReadOnlyList<KnowledgeResult>> QueryAsync(string query, int limit, CancellationToken cancellationToken = default);
}

/// <summary>
/// An in-memory <see cref="IKnowledgeBase"/> scored by lexical token overlap — deterministic,
/// network-free. The C# analog of the Rust <c>InMemoryKnowledge</c>.
/// </summary>
public sealed class InMemoryKnowledgeBase : IKnowledgeBase
{
    private readonly List<KnowledgeDocument> _docs = new();

    public Task IngestAsync(KnowledgeDocument document, CancellationToken cancellationToken = default)
    {
        _docs.RemoveAll(d => d.Id == document.Id);
        _docs.Add(document);
        return Task.CompletedTask;
    }

    public Task<IReadOnlyList<KnowledgeResult>> QueryAsync(string query, int limit, CancellationToken cancellationToken = default)
    {
        IReadOnlyList<KnowledgeResult> hits = _docs
            .Select(d => new KnowledgeResult(d.Id, d.Content, Lexical.Score(query, d.Content), d.Source))
            .Where(r => r.Score > 0)
            .OrderByDescending(r => r.Score)
            .Take(limit)
            .ToList();
        return Task.FromResult(hits);
    }
}
