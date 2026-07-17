using Microsoft.Extensions.AI;
using Tomlyn;
using Tomlyn.Model;

namespace SmooAI.SmoothOperator.Core;

/// <summary>Shared Tomlyn options — default reflection model (PascalCase ⇒ snake_case keys).</summary>
internal static class TomlOptions
{
    public static readonly TomlModelOptions Shared = new();
}

/// <summary>The reason a <see cref="IDenyPredicate"/> blocks a call.</summary>
public readonly record struct DenyReason(string Reason)
{
    /// <summary>Wrap a reason string.</summary>
    // NOTE: deliberately NOT an implicit string operator — that would let the `null` literal in a
    // `cond ? DenyReason.New(..) : null` ternary bind as `(string)null` ⇒ a bogus empty-reason deny
    // instead of "no deny". Predicates must return `null` for no-deny; use New(...) for a deny.
    public static DenyReason New(string reason) => new(reason);
}

/// <summary>
/// A consumer-supplied semantic deny check. Runs on every gated tool call; a non-null result is a
/// hard deny (circuit-breaker tier). Use it for the checks the declarative rules can't express from
/// strings alone — resolving an AWS call to its account, a DB URL to writer-vs-replica, etc.
/// </summary>
public interface IDenyPredicate
{
    /// <summary>Return a reason to deny <paramref name="call"/>, or <c>null</c> to fall through.</summary>
    DenyReason? Evaluate(FunctionCallContent call);
}

/// <summary><c>[tools]</c> — deny by tool name / glob.</summary>
public sealed class ToolsDenySection
{
    /// <summary>Glob(s) on the (possibly dotted) tool name.</summary>
    public List<string> Deny { get; set; } = new();
}

/// <summary><c>[bash]</c> — deny bash command prefixes / globs.</summary>
public sealed class BashDenySection
{
    /// <summary>Command prefixes (<c>"aws "</c>) or <c>*</c>-globs (<c>"aws * --profile prod"</c>).</summary>
    public List<string> DenyPatterns { get; set; } = new();
}

/// <summary><c>[network]</c> — deny host suffixes / globs.</summary>
public sealed class NetworkDenySection
{
    /// <summary>Host suffixes, <c>*.suffix</c> globs, or mid-string globs.</summary>
    public List<string> DenyHosts { get; set; } = new();
}

/// <summary><c>[paths]</c> — deny file paths / globs (Write + Read tools).</summary>
public sealed class PathsDenySection
{
    /// <summary>Path globs (<c>*</c>/<c>**</c> both match any run, including <c>/</c>).</summary>
    public List<string> Deny { get; set; } = new();
}

/// <summary>The declarative half of a <see cref="DenyPolicy"/>: four deny lists parsed from TOML.</summary>
public sealed class DenyRules
{
    /// <summary>Reserved for forward-compatible migrations.</summary>
    public int SchemaVersion { get; set; }

    /// <summary><c>[tools]</c> section.</summary>
    public ToolsDenySection Tools { get; set; } = new();

    /// <summary><c>[bash]</c> section.</summary>
    public BashDenySection Bash { get; set; } = new();

    /// <summary><c>[network]</c> section.</summary>
    public NetworkDenySection Network { get; set; } = new();

    /// <summary><c>[paths]</c> section.</summary>
    public PathsDenySection Paths { get; set; } = new();

    /// <summary>New empty rules pinned at the current schema version.</summary>
    public static DenyRules New() => new() { SchemaVersion = 1 };

    /// <summary>No rules in any section.</summary>
    public bool IsEmpty() =>
        Tools.Deny.Count == 0 && Bash.DenyPatterns.Count == 0 && Network.DenyHosts.Count == 0 && Paths.Deny.Count == 0;

    /// <summary>Parse from a TOML string. Missing sections default to empty.</summary>
    public static DenyRules Parse(string tomlText)
    {
        var model = Toml.ToModel<DenyRules>(tomlText, options: TomlOptions.Shared);
        model.Normalize();
        return model;
    }

    /// <summary>Serialize to pretty TOML (deterministic — lists sorted).</summary>
    public string ToTomlString()
    {
        Normalize();
        return Toml.FromModel(this, TomlOptions.Shared);
    }

    internal void Normalize()
    {
        SortDedup(Tools.Deny);
        SortDedup(Bash.DenyPatterns);
        SortDedup(Network.DenyHosts);
        SortDedup(Paths.Deny);
    }

    private static void SortDedup(List<string> list)
    {
        var seen = new SortedSet<string>(list, StringComparer.Ordinal);
        list.Clear();
        list.AddRange(seen);
    }

    /// <summary>The first declarative rule this call matches, formatted as a deny reason.</summary>
    internal string? DenyReasonFor(FunctionCallContent call)
    {
        // `[tools]` applies to ANY tool, whatever its category.
        var toolPat = Tools.Deny.FirstOrDefault(p => DenyPolicy.GlobMatch(p, call.Name));
        if (toolPat is not null)
        {
            return $"denied by policy (tools): {toolPat}";
        }
        var args = PermissionEngine.ReadArgs(call);
        switch (PermissionEngine.ToolCategory(call.Name))
        {
            case Category.Bash:
                var cmd = (PermissionEngine.ArgStr(args, "cmd") ?? PermissionEngine.ArgStr(args, "command") ?? "").Trim();
                if (cmd.Length == 0)
                {
                    return null;
                }
                var bashPat = BashDenied(cmd);
                if (bashPat is not null)
                {
                    return $"denied by policy (bash): {bashPat}";
                }
                // A denied host referenced by the command line is also blocked.
                foreach (var sub in PermissionEngine.SplitCompound(cmd))
                {
                    foreach (var host in PermissionEngine.ExtractHosts(sub))
                    {
                        var hp = HostDenied(host);
                        if (hp is not null)
                        {
                            return $"denied by policy (network): {hp}";
                        }
                    }
                }
                return null;
            case Category.Network:
                var raw = PermissionEngine.ArgStr(args, "url") ?? PermissionEngine.ArgStr(args, "host") ?? "";
                var h = PermissionEngine.HostFromToken(raw) ?? raw;
                if (h.Length == 0)
                {
                    return null;
                }
                var np = HostDenied(h);
                return np is null ? null : $"denied by policy (network): {np}";
            case Category.Write:
            case Category.Safe:
                foreach (var key in new[] { "path", "file", "dir", "directory" })
                {
                    var v = PermissionEngine.ArgStr(args, key);
                    if (v is null)
                    {
                        continue;
                    }
                    var pp = Paths.Deny.FirstOrDefault(p => DenyPolicy.GlobMatch(p, v));
                    if (pp is not null)
                    {
                        return $"denied by policy (paths): {pp}";
                    }
                }
                return null;
            default:
                return null;
        }
    }

    /// <summary>First <c>[bash]</c> pattern that matches any (wrapper/sudo-stripped) subcommand.</summary>
    private string? BashDenied(string cmd)
    {
        var subs = PermissionEngine.SplitCompound(cmd).Select(s => PermissionEngine.StripWrappersAndSudo(s).ToLowerInvariant()).ToList();
        return Bash.DenyPatterns.FirstOrDefault(pat =>
        {
            // A plain prefix (`"aws "`) gets an implicit trailing `*`; a pattern with an explicit `*`
            // also matches any trailing text so extra flags can't slip a call past the rule.
            var lower = pat.ToLowerInvariant();
            var anchored = lower.EndsWith('*') ? lower : lower + "*";
            return subs.Any(sub => DenyPolicy.GlobMatch(anchored, sub));
        });
    }

    /// <summary>First <c>[network]</c> pattern that matches <paramref name="host"/> (case-insensitive).</summary>
    private string? HostDenied(string host)
    {
        var h = host.ToLowerInvariant();
        return Network.DenyHosts.FirstOrDefault(pat => DenyPolicy.HostPatternMatches(pat, h));
    }
}

/// <summary>
/// Consumer-supplied <b>deny policy</b> — the deny-side counterpart to <see cref="PermissionGrants"/>.
///
/// <para>The engine ships hardcoded circuit-breakers and an allow-only grant store that can only
/// <i>upgrade</i> an Ask. Neither can express a consumer's own "never do this" rules. This adds that
/// tier. It is <b>purely additive</b>: a <see cref="PermissionHook"/> with no deny policy behaves
/// exactly as before. When one is attached it is evaluated <b>first</b>, and a match is a hard deny
/// of the same tier as the built-in circuit-breakers — no stored grant waives it, and
/// <see cref="AutoMode.Bypass"/> / <see cref="AutoMode.AcceptEdits"/> cannot downgrade it.</para>
///
/// <para>Two tiers: declarative <see cref="DenyRules"/> (TOML) checked first, then
/// <see cref="IDenyPredicate"/> semantic checks. The first match wins.</para>
/// </summary>
public sealed class DenyPolicy
{
    private DenyRules _declarative = new();
    private readonly List<IDenyPredicate> _predicates = new();

    /// <summary>An empty policy — denies nothing (the additive no-op default).</summary>
    public DenyPolicy() { }

    /// <summary>Build the declarative half from a TOML string.</summary>
    public static DenyPolicy FromToml(string tomlText) => new() { _declarative = DenyRules.Parse(tomlText) };

    /// <summary>Replace the declarative rules. Chainable.</summary>
    public DenyPolicy WithDeclarative(DenyRules rules)
    {
        _declarative = rules;
        return this;
    }

    /// <summary>Add a consumer predicate. Chainable.</summary>
    public DenyPolicy WithPredicate(IDenyPredicate predicate)
    {
        _predicates.Add(predicate);
        return this;
    }

    /// <summary>True when there are no rules and no predicates — nothing to deny.</summary>
    public bool IsEmpty() => _declarative.IsEmpty() && _predicates.Count == 0;

    /// <summary>
    /// The deny reason for <paramref name="call"/>, or <c>null</c> to let it fall through. Declarative
    /// rules are checked first, then predicates; the first match wins.
    /// </summary>
    public string? Evaluate(FunctionCallContent call)
    {
        var reason = _declarative.DenyReasonFor(call);
        if (reason is not null)
        {
            return reason;
        }
        foreach (var predicate in _predicates)
        {
            var r = predicate.Evaluate(call);
            if (r is { } dr)
            {
                return $"denied by policy (predicate): {dr.Reason}";
            }
        }
        return null;
    }

    // ── glob matchers (ported faithfully; kept tiny + auditable) ─────────────────────

    /// <summary>
    /// Minimal both-ends-anchored glob: <c>*</c> (and any run of <c>*</c>) matches any sequence of
    /// characters, including <c>/</c>. No <c>?</c>, no char classes.
    /// </summary>
    internal static bool GlobMatch(string pattern, string text)
    {
        var parts = pattern.Split('*');
        if (parts.Length == 1)
        {
            return pattern == text; // no wildcard → exact match
        }
        var first = parts[0];
        if (!text.StartsWith(first, StringComparison.Ordinal))
        {
            return false;
        }
        var pos = first.Length;
        var lastIdx = parts.Length - 1;
        for (var i = 1; i < parts.Length; i++)
        {
            var part = parts[i];
            if (part.Length == 0)
            {
                continue; // consecutive/trailing `*`
            }
            if (i == lastIdx)
            {
                var endStart = text.Length - part.Length;
                return endStart >= pos && text.AsSpan(pos).EndsWith(part);
            }
            var idx = text.IndexOf(part, pos, StringComparison.Ordinal);
            if (idx < 0)
            {
                return false;
            }
            pos = idx + part.Length;
        }
        // Pattern ended with `*` (last part empty): the trailing run matches anything.
        return true;
    }

    /// <summary>
    /// Match a single host deny pattern against an already-lowercased host: no <c>*</c> ⇒
    /// subdomain-aware suffix; <c>*.suffix</c> ⇒ apex + subdomains; mid-string <c>*</c> ⇒ anchored glob.
    /// </summary>
    internal static bool HostPatternMatches(string pattern, string hostLower)
    {
        var p = pattern.ToLowerInvariant();
        if (!p.Contains('*'))
        {
            return PermissionEngine.DomainMatchesSuffixList(hostLower, new[] { p });
        }
        if (p.StartsWith("*.", StringComparison.Ordinal))
        {
            var bare = p[2..];
            if (PermissionEngine.DomainMatchesSuffixList(hostLower, new[] { bare }))
            {
                return true;
            }
        }
        return GlobMatch(p, hostLower);
    }
}
