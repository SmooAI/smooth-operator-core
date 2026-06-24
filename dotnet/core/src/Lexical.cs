namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// Deterministic lexical token-overlap scoring shared by the in-memory knowledge + memory
/// stores. Phase-2 retrieval is keyword-based (no embeddings) — enough for tests and small
/// in-process corpora; an embedding-backed adapter swaps in behind the same interfaces later.
/// </summary>
internal static class Lexical
{
    /// <summary>Lowercased alphanumeric tokens of length &gt; 2 (drops noise/stopword-ish shorts).</summary>
    public static HashSet<string> Tokenize(string text)
    {
        var tokens = new HashSet<string>(StringComparer.Ordinal);
        foreach (var raw in text.ToLowerInvariant().Split((char[]?)null, StringSplitOptions.RemoveEmptyEntries))
        {
            var token = new string(raw.Where(char.IsLetterOrDigit).ToArray());
            if (token.Length > 2)
            {
                tokens.Add(token);
            }
        }
        return tokens;
    }

    /// <summary>Number of query tokens also present in <paramref name="content"/> (0 = no match).</summary>
    public static double Score(string query, string content)
    {
        var queryTokens = Tokenize(query);
        if (queryTokens.Count == 0)
        {
            return 0;
        }
        var contentTokens = Tokenize(content);
        return queryTokens.Count(contentTokens.Contains);
    }
}
