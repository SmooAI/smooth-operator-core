namespace SmooAI.SmoothOperator.Core;

/// <summary>A role's place in a multi-agent cast. Mirrors the Rust engine's <c>RoleKind</c>.</summary>
public enum RoleKind
{
    /// <summary>The orchestrator that delegates to sidekicks.</summary>
    Lead,

    /// <summary>A focused specialist a lead can dispatch a sub-task to.</summary>
    Sidekick,

    /// <summary>A passive observer (e.g. for logging/critique); not directly dispatchable.</summary>
    Shadow,
}

/// <summary>
/// Tool-access policy for a role. Mirrors the Rust engine's <c>Clearance</c>: a deny always
/// wins; a non-empty allow-list is a whitelist; empty allow + empty deny means "all tools".
/// </summary>
public sealed record Clearance
{
    public IReadOnlyList<string> AllowTools { get; init; } = Array.Empty<string>();

    public IReadOnlyList<string> DenyTools { get; init; } = Array.Empty<string>();

    /// <summary>Block every tool regardless of the lists.</summary>
    public bool DenyEverything { get; init; }

    public static Clearance AllowAll() => new();

    public static Clearance DenyAll() => new() { DenyEverything = true };

    public static Clearance Allow(params string[] tools) => new() { AllowTools = tools };

    public static Clearance Deny(params string[] tools) => new() { DenyTools = tools };

    /// <summary>Whether <paramref name="tool"/> is permitted under this clearance.</summary>
    public bool Allows(string tool)
    {
        if (DenyEverything)
        {
            return false;
        }
        if (DenyTools.Contains(tool, StringComparer.Ordinal))
        {
            return false;
        }
        if (AllowTools.Count > 0)
        {
            return AllowTools.Contains(tool, StringComparer.Ordinal);
        }
        return true;
    }
}

/// <summary>
/// A named role in the cast — its system prompt, kind, tool clearance, and iteration budget.
/// Mirrors the Rust engine's <c>OperatorRole</c>.
/// </summary>
public sealed record OperatorRole(string Name, RoleKind Kind, string Instructions)
{
    public Clearance Permissions { get; init; } = Clearance.AllowAll();

    public int MaxIterations { get; init; } = 8;

    /// <summary>Hidden from listings (still dispatchable by name).</summary>
    public bool Hidden { get; init; }
}

/// <summary>
/// The registered set of roles a lead can dispatch to. Mirrors the Rust engine's <c>Cast</c>.
/// </summary>
public sealed class Cast
{
    private readonly Dictionary<string, OperatorRole> _roles = new(StringComparer.Ordinal);

    public Cast Register(OperatorRole role)
    {
        _roles[role.Name] = role;
        return this;
    }

    public OperatorRole? Get(string name) => _roles.TryGetValue(name, out var role) ? role : null;

    public IEnumerable<OperatorRole> List() => _roles.Values;

    public IEnumerable<OperatorRole> ListVisible() => _roles.Values.Where(r => !r.Hidden);

    public IEnumerable<OperatorRole> Sidekicks() => _roles.Values.Where(r => r.Kind == RoleKind.Sidekick);

    public int Count => _roles.Count;

    public bool IsEmpty => _roles.Count == 0;
}
