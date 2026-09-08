using System.Globalization;
using System.Text;

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

/// <summary>
/// Whether a memory kind can go stale. <c>Project</c> and <c>Reference</c> entries name things
/// in a moving codebase or an external system; <c>User</c> and <c>Feedback</c> describe the
/// person, which does not change when someone renames a file.
/// </summary>
public static class MemoryTypeExtensions
{
    /// <summary>True for the time-sensitive kinds that trigger the freshness note.</summary>
    public static bool NeedsFreshnessCheck(this MemoryType type) =>
        type is MemoryType.Project or MemoryType.Reference;
}

/// <summary>A stored memory the agent can recall by relevance.</summary>
/// <remarks><c>Relevance</c> is populated by <c>RecallAsync</c> and rendered into the recall
/// block; it trails the other parameters so existing positional construction still compiles.</remarks>
public sealed record MemoryEntry(
    string Id,
    string Content,
    MemoryType Type = MemoryType.LongTerm,
    IReadOnlyDictionary<string, string>? Metadata = null,
    double Relevance = 0);

/// <summary>
/// Auto-recall behaviour shared with every sibling core.
///
/// This is a <b>cross-language contract</b>, not a local choice: the Rust reference
/// (<c>rust/smooth-operator-core/src/memory.rs</c>) and the Python, Go and TypeScript siblings
/// all produce the same block for the same memories. Pearl th-ffaeae exists because they had
/// drifted — three spellings of the header alone, plus different scores, entry formats and
/// top-k defaults, which made a shared conformance scenario impossible to write.
/// </summary>
public static class MemoryRecall
{
    /// <summary>Memories auto-recalled per turn when the caller does not set one.</summary>
    public const int MemoryTopK = 5;

    /// <summary>Header that opens every auto-recall block, in every core.</summary>
    public const string RecallHeader = "[Recalled memories]";

    /// <summary>
    /// The verify-before-recommend note (rule D6), emitted only when at least one recalled entry
    /// is time-sensitive. A memory naming a function, file, or flag is a claim about the PAST,
    /// not a fact about now — without this line the model happily recommends a symbol that was
    /// deleted three releases ago.
    /// </summary>
    public const string RecallFreshnessNote =
        "Note: 'the memory says X exists' is not the same as 'X exists now'. " +
        "Before recommending or acting on any function path, file, flag, or external " +
        "pointer named below, verify it's current by reading the file or grepping the " +
        "codebase. Project and Reference memories are time-sensitive; User and Feedback " +
        "are durable.";

    /// <summary>Lowercased alphanumeric tokens; punctuation separates rather than joins.</summary>
    /// <remarks>Deliberately NOT <c>Lexical.Tokenize</c>: that one drops tokens of length &lt;= 2,
    /// which is right for knowledge retrieval and wrong here, where the sibling cores keep them.</remarks>
    private static HashSet<string> Tokenize(string text)
    {
        var tokens = new HashSet<string>(StringComparer.Ordinal);
        var current = new StringBuilder();
        foreach (var c in text.ToLowerInvariant())
        {
            if (char.IsAsciiLetterOrDigit(c))
            {
                current.Append(c);
            }
            else if (current.Length > 0)
            {
                tokens.Add(current.ToString());
                current.Clear();
            }
        }
        if (current.Length > 0)
        {
            tokens.Add(current.ToString());
        }
        return tokens;
    }

    /// <summary>
    /// Fraction of the QUERY's distinct tokens that appear in <paramref name="content"/>.
    ///
    /// Normalised to 0–1 deliberately: a raw overlap count is comparable neither between entries
    /// nor between languages, and it is rendered into the prompt. Punctuation is a token separator
    /// — the Rust reference used to split on whitespace alone, so a query ending "…my name?" never
    /// matched a memory containing "name" and the entry was silently not recalled.
    /// </summary>
    public static double RelevanceScore(string query, string content)
    {
        var queryTokens = Tokenize(query);
        if (queryTokens.Count == 0)
        {
            return 0;
        }
        var contentTokens = Tokenize(content);
        return (double)queryTokens.Count(contentTokens.Contains) / queryTokens.Count;
    }

    /// <summary>
    /// Render recalled entries as the context block injected into a turn. Byte-identical to the
    /// Rust reference's <c>render_recall_block</c>. <c>null</c> for an empty list, so a caller
    /// injects nothing rather than a bare header telling the model it remembered nothing.
    /// </summary>
    public static string? RenderRecallBlock(IReadOnlyList<MemoryEntry> entries)
    {
        if (entries.Count == 0)
        {
            return null;
        }
        var builder = new StringBuilder();
        builder.Append(RecallHeader).Append('\n');
        if (entries.Any(e => e.Type.NeedsFreshnessCheck()))
        {
            builder.Append(RecallFreshnessNote).Append('\n');
        }
        foreach (var entry in entries)
        {
            // InvariantCulture: a comma decimal separator under a de-DE locale would make the
            // block differ from every sibling core on the same input.
            builder.Append(CultureInfo.InvariantCulture, $"- ({entry.Type}, relevance={entry.Relevance.ToString("F2", CultureInfo.InvariantCulture)}): {entry.Content}\n");
        }
        return builder.ToString();
    }
}

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
            .Select(e => (entry: e, score: MemoryRecall.RelevanceScore(query, e.Content)))
            .Where(x => x.score > 0)
            // OrderByDescending is stable in LINQ-to-objects, so ties keep insertion order —
            // the sibling cores rely on that to produce the same block.
            .OrderByDescending(x => x.score)
            .Take(limit)
            .Select(x => x.entry with { Relevance = x.score })
            .ToList();
        return Task.FromResult(hits);
    }

    public Task ForgetAsync(string id, CancellationToken cancellationToken = default)
    {
        _entries.RemoveAll(e => e.Id == id);
        return Task.CompletedTask;
    }
}
