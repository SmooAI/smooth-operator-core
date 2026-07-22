using System.Text.Json;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// How aggressively the permission gate enforces. Ported from the Rust engine's
/// <c>AutoMode</c> (a trimmed Claude-Code <c>auto-mode</c> set). Selected via the
/// <c>SMOOTH_AUTO_MODE</c> env var or <see cref="AgentOptions.WithPermissionMode"/>.
/// </summary>
public enum AutoMode
{
    /// <summary>Read-only allow, mutating ask, dangerous deny. The default.</summary>
    Ask,

    /// <summary>
    /// Like <see cref="Ask"/> but filesystem-edit tools (the Write category) auto-approve
    /// instead of asking. Everything else still follows the full engine, and the hard
    /// circuit-breakers still block. Mirrors Claude Code's <c>acceptEdits</c>.
    /// </summary>
    AcceptEdits,

    /// <summary>
    /// Like <see cref="Ask"/> but never asks — an unmatched verdict is a <b>deny</b>
    /// (fail-closed). The headless / CI posture (Claude Code's <c>dontAsk</c>).
    /// </summary>
    DenyUnmatched,

    /// <summary>
    /// Allow everything <b>except</b> the hard circuit-breakers (<c>rm -rf /</c>, dangerous
    /// domains, credential paths, pipe-to-shell, fork bombs, env dumps). Escape hatch
    /// equivalent to Claude Code's <c>bypassPermissions</c>, which keeps its circuit-breakers.
    /// </summary>
    Bypass,
}

/// <summary>Parsing helpers for <see cref="AutoMode"/>.</summary>
public static class AutoModeParser
{
    /// <summary>Parse a <c>SMOOTH_AUTO_MODE</c> value. Unknown / null → <see cref="AutoMode.Ask"/>.</summary>
    public static AutoMode FromEnvValue(string? value)
    {
        var v = value?.Trim().ToLowerInvariant().Replace("-", "").Replace("_", "");
        return v switch
        {
            "deny" or "denyunmatched" or "dontask" or "headless" => AutoMode.DenyUnmatched,
            "bypass" or "bypasspermissions" or "yolo" => AutoMode.Bypass,
            "acceptedits" or "acceptedit" or "edits" => AutoMode.AcceptEdits,
            _ => AutoMode.Ask,
        };
    }

    /// <summary>Read the mode from the process <c>SMOOTH_AUTO_MODE</c> environment variable.</summary>
    public static AutoMode FromEnv() => FromEnvValue(Environment.GetEnvironmentVariable("SMOOTH_AUTO_MODE"));
}

/// <summary>The pure verdict returned by <see cref="PermissionEngine.Decide"/>.</summary>
public abstract record Verdict
{
    private Verdict() { }

    /// <summary>Let the call through.</summary>
    public sealed record Allow : Verdict;

    /// <summary>Block the call outright. Carries a human/LLM-readable reason.</summary>
    public sealed record Deny(string Reason) : Verdict;

    /// <summary>Pause and ask a human. Carries the reason to show.</summary>
    public sealed record Ask(string Reason) : Verdict;

    /// <summary>The shared allow singleton.</summary>
    public static readonly Verdict AllowInstance = new Allow();
}

/// <summary>Tool category, derived from the tool name — drives the default posture for non-bash tools.</summary>
internal enum Category
{
    Bash,
    Network,
    Write,
    Safe,
    Unknown,
}

/// <summary>
/// The pure, deterministic permission classifier ported natively from smooth's
/// <c>smooth-bigsmooth::auto_mode</c> (and <c>smooth-narc::judge</c>). This is the
/// security-critical core — no async, no I/O — exhaustively tested. The circuit-breakers
/// (<c>rm -rf /</c>, <c>curl | sh</c>, credential-path / env-dump guards, dangerous domains,
/// compound-command splitting, sudo/wrapper stripping) always <b>deny</b>, in every mode.
/// </summary>
public static class PermissionEngine
{
    // ── circuit-breaker data (ported from smooth-narc::judge + auto_mode) ────────────

    /// <summary>Domains we never auto-approve — suffix match, case-insensitive.</summary>
    internal static readonly string[] DangerousDomainSuffixes =
    {
        ".ngrok.io", ".ngrok-free.app", "etherscan.io", "blockchain.info",
        "binance.com", "pastebin.com", "termbin.com", "transfer.sh",
    };

    /// <summary>Shell substrings that must never run — checked case-insensitively per subcommand.</summary>
    private static readonly string[] DangerousCliSubstrings =
    {
        "rm -rf /", "rm -rf ~", ":(){ :|:& };:", "mkfs", "dd if=/dev/zero of=/dev/",
        "> /dev/sda", "chmod -r 777 /", "| sudo sh", "systemctl mask",
    };

    /// <summary>Substrings meaning "this command touches a credential / sensitive path" — immediate deny.</summary>
    private static readonly string[] SensitivePathSubstrings =
    {
        ".ssh/", ".aws/credentials", ".aws/config", ".config/gh/", ".config/gcloud",
        ".gnupg", ".kube/config", ".docker/config.json", ".npmrc", ".pypirc", ".netrc",
        "/etc/shadow", "id_rsa", "id_ed25519", ".smooth/providers.json", ".smooth/auth/",
    };

    /// <summary>Read-only command binaries that are always safe. Kept tight.</summary>
    private static readonly string[] SafeBashBins =
    {
        "ls", "cat", "head", "tail", "wc", "grep", "rg", "fd", "find", "echo", "pwd",
        "which", "whoami", "date", "true", "test", "dirname", "basename", "realpath",
        "stat", "file", "cksum", "sha256sum", "md5sum",
    };

    /// <summary><c>git</c> subcommands that only read.</summary>
    private static readonly string[] SafeGitSubcommands =
    {
        "status", "log", "diff", "show", "branch", "remote", "rev-parse", "describe", "blame", "ls-files",
    };

    /// <summary>Flags under which <c>git branch</c> / <c>git remote</c> stay read-only.</summary>
    private static readonly string[] GitListOnlyFlags =
    {
        "-a", "-r", "-v", "-vv", "--all", "--list", "--verbose", "--show-current", "--merged", "--no-merged",
    };

    /// <summary>Binaries that make outbound network requests.</summary>
    private static readonly string[] NetBashBins = { "curl", "wget", "http", "https", "nc", "ncat", "telnet" };

    /// <summary>Shell interpreters that execute piped stdin — the sink half of a <c>curl … | sh</c>.</summary>
    private static readonly string[] ShellInterpreters = { "sh", "bash", "zsh", "dash", "ksh" };

    /// <summary>Env-var name fragments whose <c>$NAME</c> expansion is treated as secret exfiltration.</summary>
    private static readonly string[] SensitiveVarFragments =
    {
        "secret", "token", "password", "passwd", "api_key", "apikey", "access_key",
        "credential", "private_key", "aws_", "ssh_", "session",
    };

    /// <summary>Transparent command wrappers that don't change what runs.</summary>
    private static readonly string[] Wrappers = { "timeout", "nice", "nohup", "stdbuf", "env" };

    private static readonly char[] Whitespace = { ' ', '\t', '\n', '\r', '\f', '\v' };

    // ── argument reading ─────────────────────────────────────────────────────────────

    /// <summary>Read a string field from a tool-call argument map, tolerant of JsonElement boxing.</summary>
    internal static string? ArgStr(IReadOnlyDictionary<string, object?>? args, string key)
    {
        if (args is null || !args.TryGetValue(key, out var v) || v is null)
        {
            return null;
        }
        return v switch
        {
            string s => s,
            JsonElement je when je.ValueKind == JsonValueKind.String => je.GetString(),
            JsonElement je => je.ToString(),
            _ => v.ToString(),
        };
    }

    /// <summary>Read a tool call's arguments as a read-only map (MEAI hands us <c>IDictionary</c>).</summary>
    internal static IReadOnlyDictionary<string, object?>? ReadArgs(FunctionCallContent call)
    {
        var a = call.Arguments;
        if (a is null)
        {
            return null;
        }
        return a as IReadOnlyDictionary<string, object?> ?? new Dictionary<string, object?>(a);
    }

    private static string? FirstArg(IReadOnlyDictionary<string, object?>? args, params string[] keys)
    {
        foreach (var key in keys)
        {
            var v = ArgStr(args, key);
            if (v is not null)
            {
                return v;
            }
        }
        return null;
    }

    // ── string helpers ───────────────────────────────────────────────────────────────

    /// <summary>Match a domain against a suffix list (exact or subdomain), case-insensitive.</summary>
    internal static bool DomainMatchesSuffixList(string domain, IEnumerable<string> suffixes)
    {
        var d = domain.ToLowerInvariant();
        foreach (var suffix in suffixes)
        {
            var s = suffix.ToLowerInvariant();
            if (d == s || d.EndsWith("." + s, StringComparison.Ordinal) || (s.StartsWith('.') && d.EndsWith(s, StringComparison.Ordinal)))
            {
                return true;
            }
        }
        return false;
    }

    /// <summary>
    /// Split a shell command line into subcommands on the sequencing operators
    /// (<c>&amp;&amp;</c>, <c>||</c>, <c>;</c>, <c>|</c>, <c>&amp;</c>, newlines). Command / process
    /// substitution (<c>$(…)</c>, <c>`…`</c>, <c>&lt;(…)</c>) is surfaced as its own segment.
    /// </summary>
    internal static List<string> SplitCompound(string command)
    {
        // ponytail: substring split, not a real shell lexer — mirrors the Rust reference exactly.
        var normalized = command.Replace("&&", "").Replace("||", "");
        if (normalized.Contains("$(") || normalized.Contains("<(") || normalized.Contains('`'))
        {
            normalized = normalized.Replace("$(", "").Replace("<(", "").Replace("`", "").Replace(")", "");
        }
        return normalized
            .Split(new[] { '', ';', '|', '&', '\n' })
            .Select(s => s.Trim().Trim('"', '\'').Trim())
            .Where(s => s.Length > 0)
            .ToList();
    }

    private static string[] Tokenize(string s) => s.Split(Whitespace, StringSplitOptions.RemoveEmptyEntries);

    /// <summary>Skip leading transparent wrappers (<c>timeout 5</c>, <c>env</c>, …); return the real command index.</summary>
    private static int StripWrappers(IReadOnlyList<string> tokens)
    {
        var i = 0;
        while (i < tokens.Count && Wrappers.Contains(tokens[i]))
        {
            i++;
            while (i < tokens.Count && (tokens[i].StartsWith('-') || (tokens[i].Length > 0 && char.IsAsciiDigit(tokens[i][0]))))
            {
                i++;
            }
        }
        return i;
    }

    /// <summary>First meaningful token of a subcommand (after stripping wrappers).</summary>
    private static string? CommandBin(string subcommand)
    {
        var tokens = Tokenize(subcommand);
        var start = StripWrappers(tokens);
        return start < tokens.Length ? tokens[start] : null;
    }

    /// <summary>Pull a bare hostname out of a URL-ish or <c>host:port</c> token.</summary>
    internal static string? HostFromToken(string tok)
    {
        var schemeIdx = tok.IndexOf("://", StringComparison.Ordinal);
        var afterScheme = schemeIdx >= 0 ? tok[(schemeIdx + 3)..] : tok;
        var atIdx = afterScheme.LastIndexOf('@');
        var afterUserinfo = atIdx >= 0 ? afterScheme[(atIdx + 1)..] : afterScheme;
        var host = afterUserinfo.Split('/', ':', '?', '#')[0].Trim();
        if (host.Length == 0)
        {
            return null;
        }
        if (host == "localhost" || (host.Contains('.') && !host.StartsWith('.') && !host.EndsWith('.')))
        {
            return host.ToLowerInvariant();
        }
        return null;
    }

    /// <summary>Extract candidate hostnames from a single (already split) net-tool subcommand.</summary>
    internal static List<string> ExtractHosts(string subcommand)
    {
        var tokens = Tokenize(subcommand);
        var start = StripWrappers(tokens);
        if (start >= tokens.Length || !NetBashBins.Contains(tokens[start]))
        {
            return new List<string>();
        }
        return tokens.Skip(start + 1)
            .Where(t => !t.StartsWith('-'))
            .Select(HostFromToken)
            .Where(h => h is not null)
            .Select(h => h!)
            .ToList();
    }

    /// <summary>The effective binary of a pipe segment, skipping a leading <c>sudo</c> + wrappers.</summary>
    private static string? SinkBin(string segment)
    {
        var tokens = Tokenize(segment);
        var i = StripWrappers(tokens);
        while (i < tokens.Length && tokens[i] == "sudo")
        {
            i++;
            while (i < tokens.Length && tokens[i].StartsWith('-'))
            {
                i++;
            }
        }
        return i < tokens.Length ? tokens[i] : null;
    }

    /// <summary>Does this whole command line pipe a network fetch into a shell interpreter?</summary>
    private static bool IsPipeToShell(string command)
    {
        if (!command.Contains('|'))
        {
            return false;
        }
        var sawFetch = false;
        foreach (var seg in command.Split('|'))
        {
            var bin = SinkBin(seg.Trim());
            if (bin is null)
            {
                continue;
            }
            if (sawFetch && ShellInterpreters.Contains(bin))
            {
                return true;
            }
            if (NetBashBins.Contains(bin))
            {
                sawFetch = true;
            }
        }
        return false;
    }

    /// <summary>
    /// Strip leading transparent wrappers and any leading <c>sudo</c> from a single subcommand.
    /// Used by <see cref="DenyPolicy"/> so a rule anchored on the real binary still matches
    /// <c>sudo aws …</c> / <c>timeout 5 aws …</c>.
    /// </summary>
    internal static string StripWrappersAndSudo(string subcommand)
    {
        var tokens = Tokenize(subcommand);
        var i = StripWrappers(tokens);
        while (i < tokens.Length && tokens[i] == "sudo")
        {
            i++;
            while (i < tokens.Length && tokens[i].StartsWith('-'))
            {
                i++;
            }
        }
        return string.Join(' ', tokens.Skip(i));
    }

    private static bool ReferencesSensitivePath(string command)
    {
        var lower = command.ToLowerInvariant();
        if (SensitivePathSubstrings.Any(p => lower.Contains(p.ToLowerInvariant())))
        {
            return true;
        }
        // `.env` / `.envrc` / `.env.local` dotenv files are secret stores too. Token-scoped so
        // `rg "process.env" src/` isn't flagged.
        return Tokenize(lower).Any(t =>
        {
            var tt = t.Trim('"', '\'', '(', ')', ';');
            return tt.StartsWith(".env", StringComparison.Ordinal) || tt.Contains("/.env");
        });
    }

    private static bool ContainsSensitiveVarExpansion(string text)
    {
        var lower = text.ToLowerInvariant();
        var idx = 0;
        while (true)
        {
            var rel = lower.IndexOf('$', idx);
            if (rel < 0)
            {
                break;
            }
            var start = rel + 1;
            var j = start;
            if (j < lower.Length && lower[j] == '{')
            {
                j++;
            }
            var nameStart = j;
            while (j < lower.Length && (char.IsAsciiLetterOrDigit(lower[j]) || lower[j] == '_'))
            {
                j++;
            }
            var name = lower[nameStart..j];
            if (name.Length > 0 && SensitiveVarFragments.Any(name.Contains))
            {
                return true;
            }
            idx = start;
        }
        return false;
    }

    private static bool DumpsEnvironment(string subcommand)
    {
        var toks = Tokenize(subcommand);
        if (toks.Length == 0)
        {
            return false;
        }
        var lower = subcommand.ToLowerInvariant();
        if (lower.Contains("proc/") && lower.Contains("/environ"))
        {
            return true;
        }
        // Skip transparent wrappers (but NOT `env`, the subject here).
        var i = 0;
        while (i < toks.Length && toks[i] is "timeout" or "nice" or "nohup" or "stdbuf")
        {
            i++;
            while (i < toks.Length && (toks[i].StartsWith('-') || (toks[i].Length > 0 && char.IsAsciiDigit(toks[i][0]))))
            {
                i++;
            }
        }
        if (i >= toks.Length)
        {
            return false;
        }
        var bin = toks[i];
        var rest = toks.Skip(i + 1).ToArray();
        switch (bin)
        {
            case "printenv":
                return true;
            case "env":
                var k = 0;
                while (k < rest.Length)
                {
                    var t = rest[k];
                    if (t is "-u" or "-S")
                    {
                        k += 2;
                    }
                    else if (t.StartsWith('-') || t.Contains('=') || t == "-")
                    {
                        k += 1;
                    }
                    else
                    {
                        return false; // a bare command token → setter form
                    }
                }
                return true;
            case "export":
            case "declare":
            case "typeset":
                return !rest.Any(t => t.Contains('=')) && rest.All(t => t.StartsWith('-'));
            case "set":
                return rest.Length == 0;
            case "echo":
            case "printf":
                return ContainsSensitiveVarExpansion(subcommand);
            default:
                return false;
        }
    }

    private static bool IsSafeReadonlyBash(string subcommand)
    {
        var bin = CommandBin(subcommand);
        if (bin is null)
        {
            return false;
        }
        if (bin == "find")
        {
            string[] findActionFlags = { "-exec", "-execdir", "-ok", "-okdir", "-delete", "-fprint", "-fprintf", "-fls" };
            return !Tokenize(subcommand).Any(findActionFlags.Contains);
        }
        if (SafeBashBins.Contains(bin))
        {
            return true;
        }
        if (bin == "git")
        {
            var tokens = Tokenize(subcommand);
            var start = StripWrappers(tokens);
            var j = start + 1;
            while (j < tokens.Length && tokens[j].StartsWith('-'))
            {
                j += 2; // `-c key=val` / `-C dir`: skip flag + value.
            }
            if (j < tokens.Length)
            {
                var sub = tokens[j];
                if (!SafeGitSubcommands.Contains(sub))
                {
                    return false;
                }
                if (sub is "branch" or "remote")
                {
                    return tokens.Skip(j + 1).All(t => GitListOnlyFlags.Contains(t));
                }
                return true;
            }
            return false;
        }
        return false;
    }

    private static Verdict DecideBashSubcommand(string subcommand)
    {
        // 1. Credential-path guard — deny read AND write (exfil risk).
        if (ReferencesSensitivePath(subcommand))
        {
            return new Verdict.Deny($"command references a sensitive credential path: {subcommand}");
        }
        // 1b. Environment-dump guard — the process env is a secret store.
        if (DumpsEnvironment(subcommand))
        {
            return new Verdict.Deny($"command reveals the process environment (secret exfiltration risk): {subcommand}");
        }
        // 2. Baseline dangerous-CLI deny (rm -rf /, fork bomb, mkfs, …).
        var lower = subcommand.ToLowerInvariant();
        var needle = DangerousCliSubstrings.FirstOrDefault(n => lower.Contains(n.ToLowerInvariant()));
        if (needle is not null)
        {
            return new Verdict.Deny($"command matches dangerous-cli pattern: {needle}");
        }
        // 3. Dangerous network hosts referenced by this subcommand → deny.
        var hosts = ExtractHosts(subcommand);
        foreach (var host in hosts)
        {
            if (DomainMatchesSuffixList(host, DangerousDomainSuffixes))
            {
                return new Verdict.Deny($"{host} is on the dangerous-domain deny list");
            }
        }
        // 4. Net tool with a non-dangerous host → ask.
        if (hosts.Count > 0)
        {
            return new Verdict.Ask($"outbound request to {hosts[0]} needs approval");
        }
        // 5. Compiled-in safe read-only command → allow.
        if (IsSafeReadonlyBash(subcommand))
        {
            return Verdict.AllowInstance;
        }
        // 6. Unmatched mutating command → ask.
        var b = CommandBin(subcommand) ?? "";
        return new Verdict.Ask($"`{b}` is not a known-safe command");
    }

    private static Verdict DecideBash(string command)
    {
        // Whole-line dangerous-substring scan FIRST — some breakers contain the very operators
        // SplitCompound divides on, so they must be matched before splitting.
        var lowerLine = command.ToLowerInvariant();
        var needle = DangerousCliSubstrings.FirstOrDefault(n => lowerLine.Contains(n.ToLowerInvariant()));
        if (needle is not null)
        {
            return new Verdict.Deny($"command matches dangerous-cli pattern: {needle}");
        }
        if (IsPipeToShell(command))
        {
            return new Verdict.Deny($"pipe-to-shell execution is blocked: {command}");
        }
        var subs = SplitCompound(command);
        if (subs.Count == 0)
        {
            return new Verdict.Deny("empty command");
        }
        string? pendingAsk = null;
        foreach (var sub in subs)
        {
            switch (DecideBashSubcommand(sub))
            {
                case Verdict.Deny d:
                    return d;
                case Verdict.Ask a:
                    pendingAsk ??= a.Reason;
                    break;
            }
        }
        return pendingAsk is null ? Verdict.AllowInstance : new Verdict.Ask(pendingAsk);
    }

    internal static Category ToolCategory(string name)
    {
        // Extension tools are dotted `<ext>.<tool>`; classify on the bare tool name.
        var dotIdx = name.LastIndexOf('.');
        var bare = dotIdx >= 0 ? name[(dotIdx + 1)..] : name;
        var n = bare.ToLowerInvariant();
        if (n is "bash" or "shell" or "shell_exec" or "run_command")
        {
            return Category.Bash;
        }
        if (n.Contains("write") || n.Contains("edit") || n.Contains("delete") || n.Contains("remove") || n == "apply_patch" || n == "create_file")
        {
            return Category.Write;
        }
        if (n.Contains("fetch") || n.Contains("download") || n.StartsWith("http", StringComparison.Ordinal))
        {
            return Category.Network;
        }
        if (n.StartsWith("read", StringComparison.Ordinal) || n.StartsWith("list", StringComparison.Ordinal) || n.StartsWith("get", StringComparison.Ordinal)
            || n.Contains("search") || n == "grep" || n == "glob")
        {
            return Category.Safe;
        }
        return Category.Unknown;
    }

    private static Verdict DecideInner(string toolName, IReadOnlyDictionary<string, object?>? args)
    {
        switch (ToolCategory(toolName))
        {
            case Category.Bash:
                var cmd = (FirstArg(args, "cmd", "command") ?? "").Trim();
                return cmd.Length == 0 ? new Verdict.Deny("bash call with no command") : DecideBash(cmd);
            case Category.Safe:
                // Read-only is not exfil-proof: the read path IS the exfil path.
                foreach (var key in new[] { "path", "file", "dir", "directory" })
                {
                    var v = ArgStr(args, key);
                    if (v is not null && ReferencesSensitivePath(v))
                    {
                        return new Verdict.Deny($"{toolName} targets a sensitive credential path: {v}");
                    }
                }
                return Verdict.AllowInstance;
            case Category.Network:
                var url = FirstArg(args, "url", "host") ?? "";
                var host = HostFromToken(url) ?? url;
                if (host.Length == 0)
                {
                    return new Verdict.Deny($"{toolName} call with no url/host");
                }
                return DomainMatchesSuffixList(host, DangerousDomainSuffixes)
                    ? new Verdict.Deny($"{host} is on the dangerous-domain deny list")
                    : new Verdict.Ask($"outbound request to {host} needs approval");
            case Category.Write:
                var path = FirstArg(args, "path", "file") ?? "";
                return ReferencesSensitivePath(path)
                    ? new Verdict.Deny($"write to a sensitive credential path: {path}")
                    : new Verdict.Ask($"`{toolName}` mutates the filesystem");
            default:
                return new Verdict.Ask($"`{toolName}` is not a recognised safe tool");
        }
    }

    /// <summary>
    /// The pure, deterministic permission decision. No async, no I/O — the security-critical core.
    /// <paramref name="args"/> is the raw tool-call argument map; the relevant field is pulled per
    /// category (<c>cmd</c> for bash, <c>path</c> for writes, <c>url</c>/<c>host</c> for network).
    /// </summary>
    public static Verdict Decide(AutoMode mode, string toolName, IReadOnlyDictionary<string, object?>? args)
    {
        // Bypass still honours the hard circuit-breakers: evaluate, then downgrade any Ask to Allow —
        // Deny always survives.
        var raw = DecideInner(toolName, args);
        return (mode, raw) switch
        {
            (_, Verdict.Deny) => raw,
            (AutoMode.Bypass, _) => Verdict.AllowInstance,
            (AutoMode.AcceptEdits, Verdict.Ask) when ToolCategory(toolName) == Category.Write => Verdict.AllowInstance,
            (AutoMode.DenyUnmatched, Verdict.Ask a) => new Verdict.Deny($"headless (no interactive approver): {a.Reason}"),
            _ => raw,
        };
    }

    // ── grant derivation (map an Ask to a persistable grant; never from a Deny) ─────────

    /// <summary>
    /// The grant that "approve always" would persist for this tool call. <c>null</c> when the call
    /// is not an <see cref="Verdict.Ask"/> (already allowed, or a non-grantable Deny).
    /// </summary>
    internal static GrantQuery? GrantQueryFor(string toolName, IReadOnlyDictionary<string, object?>? args)
    {
        switch (ToolCategory(toolName))
        {
            case Category.Bash:
                var cmd = (FirstArg(args, "cmd", "command") ?? "").Trim();
                foreach (var sub in SplitCompound(cmd))
                {
                    switch (DecideBashSubcommand(sub))
                    {
                        case Verdict.Ask:
                            return BashSegmentGrant(sub);
                        case Verdict.Deny:
                            return null; // a deny sinks the line; nothing grantable
                    }
                }
                return null;
            case Category.Network:
                var url = FirstArg(args, "url", "host") ?? "";
                var host = HostFromToken(url) ?? url;
                return host.Length == 0 ? null : GrantQuery.ForNetwork(host);
            case Category.Write:
            case Category.Unknown:
                return GrantQuery.ForTool(toolName);
            default:
                return null;
        }
    }

    private static GrantQuery BashSegmentGrant(string sub)
    {
        var host = ExtractHosts(sub).FirstOrDefault();
        if (host is not null)
        {
            return GrantQuery.ForNetwork(host);
        }
        var b = CommandBin(sub) ?? "";
        return GrantQuery.ForBash($"{b} ");
    }

    /// <summary>
    /// Is this whole tool call already covered by stored grants — so the Ask can be auto-approved
    /// without prompting? For compound bash, <b>every</b> asking segment must be granted.
    /// </summary>
    internal static bool CoveredByGrants(PermissionGrants grants, string toolName, IReadOnlyDictionary<string, object?>? args)
    {
        switch (ToolCategory(toolName))
        {
            case Category.Bash:
                var cmd = (FirstArg(args, "cmd", "command") ?? "").Trim();
                var subs = SplitCompound(cmd);
                if (subs.Count == 0)
                {
                    return false;
                }
                return subs.All(sub => DecideBashSubcommand(sub) switch
                {
                    Verdict.Allow => true,
                    Verdict.Deny => false, // never auto-allow a deny
                    _ => BashSegmentGranted(sub, grants),
                });
            case Category.Network:
                var url = FirstArg(args, "url", "host") ?? "";
                var host = HostFromToken(url) ?? url;
                return host.Length > 0 && grants.MatchesHost(host);
            case Category.Write:
            case Category.Unknown:
                return grants.MatchesTool(toolName);
            default:
                return false;
        }
    }

    private static bool BashSegmentGranted(string sub, PermissionGrants grants)
    {
        var host = ExtractHosts(sub).FirstOrDefault();
        return host is not null ? grants.MatchesHost(host) : grants.MatchesBash(sub);
    }
}
