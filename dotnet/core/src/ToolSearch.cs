using System.Text.Json;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// The <c>tool_search</c> meta-tool — promotes deferred tools on demand. Mirrors the Rust
/// reference <c>tool_search.rs</c> and the sibling engines' <c>ToolSearch</c> (the behaviour, not
/// the type shapes — this core has no <c>ToolRegistry</c>; tools are a plain list on
/// <see cref="AgentOptions"/>).
///
/// As a tool set grows past ~20-30 entries, every model turn pays tokens to read schemas it won't
/// use, diluting attention. So a caller can register some tools as <b>deferred</b>
/// (<see cref="AgentOptions.DeferredTools"/>): their schemas are hidden from the model. Instead the
/// agent advertises a single built-in <c>tool_search(query)</c> meta-tool. When the model calls it,
/// this fuzzy-matches the query against the deferred tools' names + descriptions, <b>promotes</b>
/// the matches into the visible set (so the model can call them on later turns), and returns each
/// match's name + description as JSON. A deferred tool that has not been promoted is not
/// dispatchable — calling it surfaces as an unknown tool until <c>tool_search</c> promotes it.
/// </summary>
public sealed class ToolSearch
{
    /// <summary>The built-in meta-tool's name. Reserved when deferred tools are in play.</summary>
    public const string ToolName = "tool_search";

    /// <summary>
    /// Cap on how many deferred tools one <c>tool_search</c> call may promote, so a generic query
    /// like "tool" doesn't promote the entire deferred set in one shot.
    /// </summary>
    public const int MaxMatches = 8;

    private const string Description =
        "Search for additional tools by keyword. Returns matching tool schemas as JSON; " +
        "matched tools become available on subsequent turns. Use when you think a tool exists " +
        "for a specific task but isn't in your current tool list — e.g. tool_search(query: \"git\") " +
        "or tool_search(query: \"http request\").";

    private readonly Dictionary<string, AIFunction> _deferredByName;
    private readonly HashSet<string> _promoted = new(StringComparer.Ordinal);
    private readonly AIFunction _metaTool;

    public ToolSearch(IEnumerable<AIFunction> deferred)
    {
        _deferredByName = deferred.ToDictionary(t => t.Name, StringComparer.Ordinal);
        _metaTool = AIFunctionFactory.Create(Search, ToolName, Description);
    }

    /// <summary>The <c>tool_search</c> meta-tool the agent advertises and dispatches.</summary>
    public AIFunction MetaTool => _metaTool;

    /// <summary>True if any tool was registered deferred (the meta-tool is advertised only then).</summary>
    public bool HasDeferred => _deferredByName.Count > 0;

    /// <summary>True if a deferred tool has been promoted and is now dispatchable.</summary>
    public bool IsPromoted(string name) => _promoted.Contains(name);

    /// <summary>The deferred tools that have been promoted — their schemas join the visible set.</summary>
    public IReadOnlyList<AIFunction> PromotedTools() =>
        _promoted.Select(n => _deferredByName.TryGetValue(n, out var t) ? t : null)
            .Where(t => t is not null)
            .Cast<AIFunction>()
            .ToList();

    /// <summary>Resolve a promoted deferred tool for dispatch. Unpromoted deferred tools are invisible.</summary>
    public AIFunction? ResolvePromoted(string name) =>
        _promoted.Contains(name) && _deferredByName.TryGetValue(name, out var t) ? t : null;

    /// <summary>Mark a deferred tool promoted. Returns false if no such deferred tool.</summary>
    public bool Promote(string name)
    {
        if (!_deferredByName.ContainsKey(name))
        {
            return false;
        }
        _promoted.Add(name);
        return true;
    }

    /// <summary>Fuzzy-match the query, promote matches, and return their schemas as JSON.</summary>
    private string Search(string query)
    {
        var needle = (query ?? string.Empty).Trim().ToLowerInvariant();
        if (needle.Length == 0)
        {
            return JsonSerializer.Serialize(new { matched = 0, tools = Array.Empty<object>(), note = "empty query — pass a keyword like \"git\" or \"network\"" });
        }

        var matched = new List<AIFunction>();
        foreach (var tool in _deferredByName.Values)
        {
            if (tool.Name.ToLowerInvariant().Contains(needle) || (tool.Description ?? string.Empty).ToLowerInvariant().Contains(needle))
            {
                matched.Add(tool);
                if (matched.Count >= MaxMatches)
                {
                    break;
                }
            }
        }

        foreach (var tool in matched)
        {
            _promoted.Add(tool.Name);
        }

        var tools = matched.Select(t => new { name = t.Name, description = t.Description, parameters = t.JsonSchema }).ToList();
        return JsonSerializer.Serialize(new { matched = tools.Count, tools });
    }
}
