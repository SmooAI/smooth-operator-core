using Tomlyn;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// Persistent permission grants — <c>wonk-allow.toml</c> (ported from the Rust engine's
/// <c>permission_grants</c>). The gate closes an <see cref="Verdict.Ask"/> by prompting a human;
/// without persistence that prompt is <i>approve-once</i>. A stored grant that matches a later Ask
/// auto-approves it <b>without prompting</b>.
///
/// <para>Two TOML files stack at load time (project wins on collision — though for pure union
/// allow-lists "wins" only affects <c>schema_version</c>):</para>
/// <list type="bullet">
/// <item><c>~/.smooth/wonk-allow.toml</c> — the user's personal grants.</item>
/// <item><c>&lt;repo&gt;/.smooth/wonk-allow.toml</c> — project-scoped grants.</item>
/// </list>
///
/// <para>There is no deny section: a stored grant can only upgrade an Ask, <b>never</b> waive a
/// Deny circuit-breaker.</para>
/// </summary>
public sealed class PermissionGrants
{
    /// <summary>Always 1. Reserved for forward-compatible migrations.</summary>
    public int SchemaVersion { get; set; }

    /// <summary><c>[network]</c> — hosts (or <c>*.suffix</c> globs) approved without asking.</summary>
    public NetworkGrantSection Network { get; set; } = new();

    /// <summary><c>[tools]</c> — tool names approved without asking (exact match).</summary>
    public ToolsGrantSection Tools { get; set; } = new();

    /// <summary><c>[bash]</c> — command prefixes approved without asking.</summary>
    public BashGrantSection Bash { get; set; } = new();

    /// <summary>New grants pinned at the current schema version.</summary>
    public static PermissionGrants New() => new() { SchemaVersion = 1 };

    /// <summary>True if <paramref name="host"/> is covered by the <c>[network]</c> allow-list.</summary>
    public bool MatchesHost(string host)
    {
        var lower = host.ToLowerInvariant();
        return Network.AllowHosts.Any(pat => HostMatchesGlob(lower, pat));
    }

    /// <summary>True if <paramref name="toolName"/> is in the <c>[tools]</c> allow-list (exact match).</summary>
    public bool MatchesTool(string toolName) => Tools.Allow.Contains(toolName);

    /// <summary>True if <paramref name="command"/> starts with any <c>[bash]</c> allow prefix.</summary>
    public bool MatchesBash(string command)
    {
        var lower = command.ToLowerInvariant();
        return Bash.AllowPatterns.Any(p => lower.StartsWith(p.ToLowerInvariant(), StringComparison.Ordinal));
    }

    /// <summary>True if <paramref name="query"/>'s exact entry is already stored.</summary>
    public bool Contains(GrantQuery query) => query.Kind switch
    {
        GrantKind.Network => MatchesHost(query.Value),
        GrantKind.Tool => MatchesTool(query.Value),
        _ => MatchesBash(query.Value),
    };

    /// <summary>Add a grant. Idempotent.</summary>
    public void Add(GrantQuery query)
    {
        switch (query.Kind)
        {
            case GrantKind.Network:
                AddSorted(Network.AllowHosts, query.Value);
                break;
            case GrantKind.Tool:
                AddSorted(Tools.Allow, query.Value);
                break;
            default:
                AddSorted(Bash.AllowPatterns, query.Value);
                break;
        }
    }

    /// <summary>Union <paramref name="other"/> into <c>this</c>.</summary>
    public void MergeWith(PermissionGrants other)
    {
        SchemaVersion = Math.Max(SchemaVersion, other.SchemaVersion);
        foreach (var h in other.Network.AllowHosts)
        {
            AddSorted(Network.AllowHosts, h);
        }
        foreach (var t in other.Tools.Allow)
        {
            AddSorted(Tools.Allow, t);
        }
        foreach (var b in other.Bash.AllowPatterns)
        {
            AddSorted(Bash.AllowPatterns, b);
        }
    }

    /// <summary>Parse from a TOML string. Missing sections default to empty.</summary>
    public static PermissionGrants Parse(string tomlText)
    {
        var model = Toml.ToModel<PermissionGrants>(tomlText, options: TomlOptions.Shared);
        model.Normalize();
        return model;
    }

    /// <summary>Serialize to pretty TOML (deterministic — lists sorted).</summary>
    public string ToTomlString()
    {
        Normalize();
        return Toml.FromModel(this, TomlOptions.Shared);
    }

    /// <summary>
    /// Load from <paramref name="path"/>. A missing file yields an empty (v1) store — <b>not</b> an
    /// error. A malformed file surfaces the parse error.
    /// </summary>
    public static PermissionGrants LoadFromPath(string path)
    {
        if (!File.Exists(path))
        {
            return New();
        }
        try
        {
            return Parse(File.ReadAllText(path));
        }
        catch (Exception e) when (e is not IOException and not UnauthorizedAccessException)
        {
            throw new InvalidOperationException($"malformed wonk-allow.toml at {path}: {e.Message}", e);
        }
    }

    /// <summary>Load user + project files and merge them (project last so its schema_version wins).</summary>
    public static PermissionGrants LoadLayered(string? user, string? project)
    {
        var merged = New();
        if (user is not null)
        {
            merged.MergeWith(LoadFromPath(user));
        }
        if (project is not null)
        {
            merged.MergeWith(LoadFromPath(project));
        }
        return merged;
    }

    /// <summary>Atomically write to <paramref name="path"/> (tempfile + rename), creating parent dirs.</summary>
    public void SaveToPath(string path)
    {
        var parent = Path.GetDirectoryName(path);
        if (!string.IsNullOrEmpty(parent))
        {
            Directory.CreateDirectory(parent);
        }
        var tmp = path + ".tmp";
        File.WriteAllText(tmp, ToTomlString());
        File.Move(tmp, path, overwrite: true);
    }

    /// <summary>The user-scope grants file: <c>~/.smooth/wonk-allow.toml</c>. <c>null</c> with no home dir.</summary>
    public static string? UserGrantsPath()
    {
        var home = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
        return string.IsNullOrEmpty(home) ? null : Path.Combine(home, ".smooth", "wonk-allow.toml");
    }

    /// <summary>The project-scope grants file: <c>&lt;workspace&gt;/.smooth/wonk-allow.toml</c>.</summary>
    public static string ProjectGrantsPath(string workspace) => Path.Combine(workspace, ".smooth", "wonk-allow.toml");

    /// <summary>Load the grant at <paramref name="path"/>, add <paramref name="query"/>, atomically save.</summary>
    public static void AppendGrant(string path, GrantQuery query)
    {
        var grants = LoadFromPath(path);
        if (grants.SchemaVersion == 0)
        {
            grants.SchemaVersion = 1;
        }
        grants.Add(query);
        grants.SaveToPath(path);
    }

    /// <summary>Sort + dedup the section lists (deterministic file output, matching the Rust BTreeSet).</summary>
    internal void Normalize()
    {
        SortDedup(Network.AllowHosts);
        SortDedup(Tools.Allow);
        SortDedup(Bash.AllowPatterns);
    }

    private static void AddSorted(List<string> list, string value)
    {
        if (!list.Contains(value))
        {
            list.Add(value);
            list.Sort(StringComparer.Ordinal);
        }
    }

    private static void SortDedup(List<string> list)
    {
        var seen = new SortedSet<string>(list, StringComparer.Ordinal);
        list.Clear();
        list.AddRange(seen);
    }

    /// <summary>
    /// Glob match for a single host pattern (case-insensitive): exact host, <c>*.example.com</c> /
    /// <c>.example.com</c> (any subdomain + the bare apex), or a bare suffix (exact only).
    /// </summary>
    public static bool HostMatchesGlob(string host, string pattern)
    {
        var h = host.ToLowerInvariant();
        var p = pattern.ToLowerInvariant();
        if (h == p)
        {
            return true;
        }
        if (p.StartsWith("*.", StringComparison.Ordinal))
        {
            var suffix = p[2..];
            return h.EndsWith("." + suffix, StringComparison.Ordinal) || h == suffix;
        }
        if (p.StartsWith('.'))
        {
            var suffix = p[1..];
            return h.EndsWith("." + suffix, StringComparison.Ordinal) || h == suffix;
        }
        return false;
    }
}

/// <summary><c>[network]</c> grant section.</summary>
public sealed class NetworkGrantSection
{
    /// <summary>Hosts (or <c>*.suffix</c> globs) approved without asking.</summary>
    public List<string> AllowHosts { get; set; } = new();
}

/// <summary><c>[tools]</c> grant section.</summary>
public sealed class ToolsGrantSection
{
    /// <summary>Tool names approved without asking (exact match).</summary>
    public List<string> Allow { get; set; } = new();
}

/// <summary><c>[bash]</c> grant section.</summary>
public sealed class BashGrantSection
{
    /// <summary>Command prefixes approved without asking. <c>"cargo "</c> matches <c>cargo test</c> etc.</summary>
    public List<string> AllowPatterns { get; set; } = new();
}

/// <summary>The kind of resource a <see cref="GrantQuery"/> covers.</summary>
public enum GrantKind
{
    /// <summary>A network host (or <c>*.suffix</c> glob).</summary>
    Network,

    /// <summary>An exact tool name (write / unknown tool).</summary>
    Tool,

    /// <summary>A bash command prefix, e.g. <c>"npm "</c>.</summary>
    Bash,
}

/// <summary>
/// One of the three grantable Ask shapes. (Deny circuit-breakers are never grantable.)
/// </summary>
public readonly record struct GrantQuery(GrantKind Kind, string Value)
{
    /// <summary>A network host grant.</summary>
    public static GrantQuery ForNetwork(string host) => new(GrantKind.Network, host);

    /// <summary>An exact tool-name grant.</summary>
    public static GrantQuery ForTool(string name) => new(GrantKind.Tool, name);

    /// <summary>A bash command-prefix grant.</summary>
    public static GrantQuery ForBash(string prefix) => new(GrantKind.Bash, prefix);
}

/// <summary>
/// Thread-safe, cheaply-shared handle to the live merged grants. Reads take a snapshot;
/// approve-always merges the freshly-persisted grant back in.
/// </summary>
public sealed class SharedGrants
{
    private readonly object _lock = new();
    private PermissionGrants _inner;

    /// <summary>Wrap an initial grant set.</summary>
    public SharedGrants(PermissionGrants grants) => _inner = grants;

    /// <summary>A snapshot (deep-copied via TOML round-trip is overkill — matching is read-only on a stable clone).</summary>
    public PermissionGrants Snapshot()
    {
        lock (_lock)
        {
            var copy = PermissionGrants.New();
            copy.MergeWith(_inner);
            return copy;
        }
    }

    /// <summary>Union <paramref name="other"/> into the live grants.</summary>
    public void MergeIn(PermissionGrants other)
    {
        lock (_lock)
        {
            _inner.MergeWith(other);
        }
    }
}
