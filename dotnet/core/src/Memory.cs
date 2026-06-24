namespace SmooAI.SmoothOperator.Core;

/// <summary>Category of a remembered fact. Mirrors the Rust <c>MemoryType</c>.</summary>
public enum MemoryType
{
    ShortTerm,
    LongTerm,
    Entity,
    User,
    Feedback,
    Project,
    Reference,
}

/// <summary>A stored memory the agent can recall by relevance.</summary>
public sealed record MemoryEntry(
    string Id,
    string Content,
    MemoryType Type = MemoryType.LongTerm,
    IReadOnlyDictionary<string, string>? Metadata = null);

/// <summary>
/// Pluggable long-/short-term memory the agent recalls from before answering. Mirrors the
/// Rust engine's <c>Memory</c> trait. The bundled <see cref="InMemoryAgentMemory"/> does
/// deterministic lexical recall for tests and small in-process use.
/// </summary>
public interface IAgentMemory
{
    Task StoreAsync(MemoryEntry entry, CancellationToken cancellationToken = default);

    Task<IReadOnlyList<MemoryEntry>> RecallAsync(string query, int limit, CancellationToken cancellationToken = default);

    Task ForgetAsync(string id, CancellationToken cancellationToken = default);
}

/// <summary>
/// An in-memory <see cref="IAgentMemory"/> scored by lexical token overlap. The C# analog of
/// the Rust <c>InMemoryMemory</c>.
/// </summary>
public sealed class InMemoryAgentMemory : IAgentMemory
{
    private readonly List<MemoryEntry> _entries = new();

    public Task StoreAsync(MemoryEntry entry, CancellationToken cancellationToken = default)
    {
        _entries.RemoveAll(e => e.Id == entry.Id);
        _entries.Add(entry);
        return Task.CompletedTask;
    }

    public Task<IReadOnlyList<MemoryEntry>> RecallAsync(string query, int limit, CancellationToken cancellationToken = default)
    {
        IReadOnlyList<MemoryEntry> hits = _entries
            .Select(e => (entry: e, score: Lexical.Score(query, e.Content)))
            .Where(x => x.score > 0)
            .OrderByDescending(x => x.score)
            .Take(limit)
            .Select(x => x.entry)
            .ToList();
        return Task.FromResult(hits);
    }

    public Task ForgetAsync(string id, CancellationToken cancellationToken = default)
    {
        _entries.RemoveAll(e => e.Id == id);
        return Task.CompletedTask;
    }
}
