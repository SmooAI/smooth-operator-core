using System.Text;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// Prompt caching — the static/dynamic system-prompt split. The C# port of the Rust
/// reference engine's <c>conversation::PromptCache</c>.
///
/// <para>A system prompt has two halves with very different churn rates: role
/// instructions and tool schemas barely change, while project context (AGENTS.md /
/// CLAUDE.md, the working set) changes every turn. Anthropic's prompt cache keys on a
/// <i>prefix</i>, so putting the volatile half first invalidates the whole thing.
/// <see cref="Boundary"/> splits them: everything above the marker is static and hashed
/// once for cache-key dedup, everything below is dynamic and can be swapped without
/// busting the static prefix.</para>
///
/// <para>Feed the result to the agent as its instructions:
/// <c>new SmoothAgent(client, new AgentOptions { Instructions = cache.FullPrompt() })</c>.</para>
/// </summary>
public sealed class PromptCache
{
    /// <summary>
    /// Marker splitting a system prompt into a cacheable static portion and a
    /// frequently-changing dynamic portion.
    /// </summary>
    public const string Boundary = "__PROMPT_CACHE_BOUNDARY__";

    private const ulong FnvOffset64 = 14695981039346656037;
    private const ulong FnvPrime64 = 1099511628211;

    /// <summary>The cacheable half (above the marker).</summary>
    public string StaticPortion { get; }

    /// <summary>The frequently-changing half (below the marker).</summary>
    public string DynamicPortion { get; private set; }

    private readonly string _staticHash;
    private readonly int _staticTokens;

    /// <summary>
    /// Split a system prompt at the boundary marker. With no marker the entire prompt is
    /// treated as dynamic — nothing is claimed cacheable that the caller didn't mark.
    /// </summary>
    public PromptCache(string prompt)
    {
        ArgumentNullException.ThrowIfNull(prompt);

        var idx = prompt.IndexOf(Boundary, StringComparison.Ordinal);
        if (idx < 0)
        {
            StaticPortion = string.Empty;
            DynamicPortion = prompt;
        }
        else
        {
            StaticPortion = prompt[..idx];
            DynamicPortion = prompt[(idx + Boundary.Length)..];
        }

        _staticHash = HashPromptPortion(StaticPortion);
        _staticTokens = StaticPortion.Length == 0 ? 0 : StaticPortion.Length / 4 + 1;
    }

    /// <summary>
    /// Reassemble static + boundary + dynamic. With no static portion the dynamic half is
    /// returned alone, so a prompt that was never split round-trips unchanged rather than
    /// gaining a stray marker.
    /// </summary>
    public string FullPrompt() => StaticPortion.Length == 0 ? DynamicPortion : StaticPortion + Boundary + DynamicPortion;

    /// <summary>
    /// Swap the dynamic half, leaving the static half and its hash untouched — the whole
    /// point of the split.
    /// </summary>
    public void UpdateDynamic(string dynamic) => DynamicPortion = dynamic;

    /// <summary>
    /// Identifies the static portion for cache-key deduplication.
    ///
    /// <para>Process-local only: compared against other hashes from THIS engine, never sent
    /// on the wire, so it deliberately does not match the Rust reference's value (Rust uses
    /// <c>DefaultHasher</c>, which is not reproducible across languages — or even across Rust
    /// releases). The ported contract is the behavior: same static text hashes the same,
    /// different static text hashes differently, and <see cref="UpdateDynamic"/> never
    /// changes it.</para>
    /// </summary>
    public string StaticHash() => _staticHash;

    /// <summary>Estimated tokens the static portion saves on a cache hit.</summary>
    public int CachedTokens() => _staticTokens;

    /// <summary>
    /// FNV-1a (64-bit), the same non-cryptographic hash family the vector embedder uses,
    /// rendered as 16 hex chars like the Rust reference.
    /// </summary>
    private static string HashPromptPortion(string s)
    {
        var h = FnvOffset64;
        foreach (var b in Encoding.UTF8.GetBytes(s))
        {
            h = unchecked((h ^ b) * FnvPrime64);
        }
        return h.ToString("x16", System.Globalization.CultureInfo.InvariantCulture);
    }
}
