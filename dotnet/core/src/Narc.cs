using System.Text;
using System.Text.Json;
using System.Text.RegularExpressions;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// Severity of a Narc finding, ordered least → most severe. A <see cref="Severity.Block"/> finding in
/// <see cref="NarcHook.PreCallAsync"/> blocks the tool call. The numeric values ARE the ordering —
/// compare with <c>&gt;=</c>.
/// </summary>
public enum Severity
{
    /// <summary>Informational — no action.</summary>
    Info = 0,

    /// <summary>Suspicious but plausibly legitimate (e.g. a secret in an argument).</summary>
    Warn = 1,

    /// <summary>Strong signal worth surfacing, but not auto-blocked.</summary>
    Alert = 2,

    /// <summary>Actively harmful — blocks the call when raised in <c>PreCall</c>.</summary>
    Block = 3,
}

/// <summary>Severity helpers.</summary>
public static class SeverityExtensions
{
    /// <summary>The shared wire label (<c>INFO</c>/<c>WARN</c>/<c>ALERT</c>/<c>BLOCK</c>) used by the cross-language corpus.</summary>
    public static string Label(this Severity s) => s switch
    {
        Severity.Warn => "WARN",
        Severity.Alert => "ALERT",
        Severity.Block => "BLOCK",
        _ => "INFO",
    };
}

/// <summary>
/// A single surveillance finding. Lean by design — the consumer supplies the timestamp and
/// correlation, so no id/timestamp fields are carried.
/// </summary>
/// <param name="Severity">How severe the finding is.</param>
/// <param name="Category">Coarse bucket: <c>injection</c>, <c>secret</c>, <c>secret_leak</c>, <c>injection_output</c>.</param>
/// <param name="PatternName">The named pattern that matched.</param>
/// <param name="Redacted">Redacted view of the matched text (never the raw secret).</param>
/// <param name="ToolName">The tool whose args/result triggered the finding.</param>
public sealed record NarcAlert(Severity Severity, string Category, string PatternName, string Redacted, string ToolName);

/// <summary>A pattern match: which pattern, its severity, and a redacted view.</summary>
/// <param name="PatternName">The named pattern that matched.</param>
/// <param name="Severity">The finding's severity.</param>
/// <param name="Redacted">Redacted view of the matched text (safe to log).</param>
public sealed record NarcFinding(string PatternName, Severity Severity, string Redacted);

/// <summary>
/// The secret + prompt-injection detectors — the .NET port of the Rust reference engine's
/// <c>narc.rs</c> scanners. The detection set is pinned across all five engines by the shared corpus
/// at <c>spec/narc/corpus.json</c>; if a port disagrees with Rust, the port is wrong.
/// </summary>
public static class Narc
{
    private sealed record NamedPattern(string Name, Severity Severity, Regex Regex);

    private const RegexOptions Opts = RegexOptions.Compiled | RegexOptions.CultureInvariant;
    private const RegexOptions IgnoreCase = Opts | RegexOptions.IgnoreCase;

    /// <summary>
    /// The 10 secret patterns. All are <see cref="Severity.Warn"/> in arguments (may be legit) and
    /// escalate to <see cref="Severity.Block"/> when found in a result (a leak) — the caller decides
    /// which threshold to apply.
    /// </summary>
    private static readonly NamedPattern[] SecretPatterns =
    [
        new("AWS Access Key", Severity.Warn, new Regex(@"AKIA[0-9A-Z]{16}", Opts)),
        new("AWS Secret Key", Severity.Warn, new Regex(@"aws[_\-]?secret[_\-]?access[_\-]?key\s*[=:]\s*[A-Za-z0-9/+=]{40}", IgnoreCase)),
        new("Anthropic API Key", Severity.Warn, new Regex(@"sk-ant-[A-Za-z0-9\-_]{20,}", Opts)),
        new("OpenAI API Key", Severity.Warn, new Regex(@"sk-[A-Za-z0-9]{20,}", Opts)),
        new("GitHub Token", Severity.Warn, new Regex(@"gh[posr]_[A-Za-z0-9_]{36,}", Opts)),
        new("Private Key", Severity.Warn, new Regex(@"-----BEGIN\s+(RSA\s+)?PRIVATE\s+KEY-----", Opts)),
        new("Generic Secret", Severity.Warn, new Regex(@"(secret|password|token|api[_\-]?key)\s*[=:]\s*[""']?[A-Za-z0-9/+=\-_]{8,}", IgnoreCase)),
        new("Bearer Token", Severity.Warn, new Regex(@"Bearer\s+[A-Za-z0-9\-_.~+/]+=*", Opts)),
        new("Base64 Encoded Key", Severity.Warn, new Regex(@"(key|secret|password)\s*[=:]\s*[A-Za-z0-9+/]{32,}={0,2}", IgnoreCase)),
        new("Stripe Key", Severity.Warn, new Regex(@"[sr]k_(live|test)_[A-Za-z0-9]{20,}", Opts)),
    ];

    /// <summary>
    /// The 8 injection patterns. Only the active data/URL exfiltration signals are
    /// <see cref="Severity.Block"/> (blocked in arguments); hijack/jailbreak text is
    /// <see cref="Severity.Alert"/> (surveilled, not blocked — it can appear in legitimate content the
    /// model is authoring, e.g. a security test or documentation about injection).
    /// </summary>
    private static readonly NamedPattern[] InjectionPatterns =
    [
        new("ignore_instructions", Severity.Alert, new Regex(@"ignore\s+(all\s+)?(previous|prior|above)\s+(instructions|prompts|rules)", IgnoreCase)),
        new("role_hijack", Severity.Alert, new Regex(@"(you\s+are\s+now|act\s+as|pretend\s+(to\s+be|you\s+are)|from\s+now\s+on\s+you\s+are)", IgnoreCase)),
        new("system_prompt", Severity.Alert, new Regex(@"(system\s*:\s*|<\|system\|>|\[SYSTEM\])", IgnoreCase)),
        new("jailbreak", Severity.Alert, new Regex(@"(DAN\s+mode|developer\s+mode|do\s+anything\s+now|jailbreak)", IgnoreCase)),
        new("base64_smuggling", Severity.Alert, new Regex(@"(decode|eval|execute)\s+(this\s+)?(base64|encoded)", IgnoreCase)),
        new("data_exfiltration", Severity.Block, new Regex(
            """
            (send|post|upload|exfiltrate|transmit|leak|push)
            \s+
            (all\s+|the\s+|our\s+|my\s+|this\s+)*
            (
                data|files?|secrets?|credentials?|keys?|tokens?|
                contents?|env\s+(vars?|file)|
                package\.json|\.env|pyproject\.toml|cargo\.toml|
                requirements\.txt|gemfile|go\.mod|composer\.json|
                \.ssh/[a-z_]+|id_rsa|\.aws/[a-z]+|\.gnupg/
            )
            \s+(to|via|at|over)
            """,
            IgnoreCase | RegexOptions.IgnorePatternWhitespace)),
        new("url_exfiltration", Severity.Block, new Regex(
            @"(send|post|upload|push|transmit|leak|exfiltrate)\b[^.\n]{1,200}\s+(to|via|at|over)\s+(https?://[\w.\-/]+)", IgnoreCase)),
        new("smell_url", Severity.Alert, new Regex(
            @"https?://[\w.\-]*\b(leak|exfil|attacker|evil|tracker|c2(?:server)?|webhook\.site)\b[\w.\-/]*", IgnoreCase)),
    ];

    private static List<NarcFinding> Scan(NamedPattern[] patterns, string text)
    {
        var findings = new List<NarcFinding>();
        foreach (var p in patterns)
        {
            foreach (Match m in p.Regex.Matches(text))
            {
                findings.Add(new NarcFinding(p.Name, p.Severity, RedactMatch(m.Value)));
            }
        }

        return findings;
    }

    /// <summary>Scan <paramref name="text"/> for hardcoded secrets. Every match is redacted.</summary>
    public static IReadOnlyList<NarcFinding> ScanSecrets(string text) => Scan(SecretPatterns, text);

    /// <summary>Scan <paramref name="text"/> for prompt-injection patterns. Matched text is redacted.</summary>
    public static IReadOnlyList<NarcFinding> ScanInjection(string text) => Scan(InjectionPatterns, text);

    /// <summary>True if <paramref name="text"/> contains any secret pattern.</summary>
    public static bool HasSecrets(string text) => SecretPatterns.Any(p => p.Regex.IsMatch(text));

    /// <summary>True if <paramref name="text"/> contains any injection pattern.</summary>
    public static bool HasInjection(string text) => InjectionPatterns.Any(p => p.Regex.IsMatch(text));

    /// <summary>
    /// Redact a matched string, showing only the first 4 and last 2 characters. Short matches
    /// (≤ 8 code points) are fully starred. Code points, not UTF-16 units — parity with the Rust
    /// reference's char-based redaction.
    /// </summary>
    public static string RedactMatch(string s)
    {
        var runes = s.EnumerateRunes().ToArray();
        if (runes.Length <= 8)
        {
            return new string('*', runes.Length);
        }

        var sb = new StringBuilder();
        foreach (var r in runes.Take(4))
        {
            sb.Append(r);
        }

        sb.Append('*', runes.Length - 6).Append("**");
        foreach (var r in runes.Skip(runes.Length - 2))
        {
            sb.Append(r);
        }

        return sb.ToString();
    }

    /// <summary>
    /// Replace every secret match in <paramref name="content"/> with <c>[REDACTED:&lt;pattern-name&gt;]</c>.
    /// Used by <see cref="NarcHook.PostCallAsync"/> on the leak path.
    /// </summary>
    internal static string RedactSecrets(string content)
    {
        foreach (var p in SecretPatterns)
        {
            // A MatchEvaluator so `$`-sequences in a pattern name could never be interpolated.
            var replacement = $"[REDACTED:{p.Name}]";
            content = p.Regex.Replace(content, _ => replacement);
        }

        return content;
    }
}

/// <summary>
/// Native secret-detection + prompt-injection scanning <see cref="IToolHook"/> — the .NET port of the
/// Rust reference engine's <c>narc.rs</c> (pearl th-5f7227).
///
/// <para>The SEP extension host passes tool-call arguments to the extension subprocess <b>unscanned</b>
/// and returns the subprocess's tool-result content to the model <b>verbatim</b>. Nothing at the
/// extension boundary looks for leaked credentials or prompt-injection payloads. This hook closes that
/// gap: it scans <b>secrets</b> (10 credential patterns) and <b>prompt injection</b> (8 patterns).</para>
///
/// <para><b>Division of labour with <see cref="PermissionHook"/>:</b> the permission gate already owns
/// the dangerous-command / write / credential-path circuit-breakers (<c>rm -rf /</c>, <c>curl | sh</c>,
/// <c>~/.ssh/id_rsa</c>). Narc does NOT re-implement those — it is scoped to the one thing permission
/// does not do: <b>content scanning of arguments and results</b>. Install it in
/// <c>AgentOptions.ToolHooks</c>, which the agent runs after the permission gate.</para>
///
/// <para><b>PreCall (arguments)</b> throws on a <see cref="Severity.Block"/> injection pattern (the
/// active data/URL exfiltration signals), blocking the call before the tool runs. Lower-severity
/// injection and any secret in the arguments are <b>alerted, not blocked</b> — a tool argument
/// legitimately carrying a secret (writing a <c>.env</c>, configuring a client) is common enough that a
/// hard block there would be a footgun.</para>
///
/// <para><b>PostCall (result)</b> detects, alerts, and <b>redacts</b>. A secret in a tool result is a
/// leak: it raises a <see cref="Severity.Block"/> alert AND replaces the matched credential with
/// <c>[REDACTED:&lt;pattern-name&gt;]</c> in <see cref="FunctionResultContent.Result"/> before it
/// reaches the model. Injection in the result stays detection + <see cref="Severity.Alert"/> only
/// (surveillance) — it can appear in legitimate content and is never rewritten.</para>
///
/// <para>Safe for concurrent use: with parallel tool calls the agent may run Pre/Post from several
/// tasks at once.</para>
/// </summary>
public sealed class NarcHook : IToolHook
{
    private readonly List<NarcAlert> _alerts = [];
    private readonly object _gate = new();

    /// <summary>Snapshot every recorded alert.</summary>
    public IReadOnlyList<NarcAlert> Alerts()
    {
        lock (_gate)
        {
            return _alerts.ToList();
        }
    }

    /// <summary>Recorded alerts at or above <paramref name="minSeverity"/>.</summary>
    public IReadOnlyList<NarcAlert> AlertsAbove(Severity minSeverity)
    {
        lock (_gate)
        {
            return _alerts.Where(a => a.Severity >= minSeverity).ToList();
        }
    }

    private void Record(NarcAlert alert)
    {
        if (alert.Severity >= Severity.Alert)
        {
            Console.Error.WriteLine(
                $"narc: {alert.Severity.Label()} finding tool={alert.ToolName} category={alert.Category} pattern={alert.PatternName} redacted={alert.Redacted}");
        }

        lock (_gate)
        {
            _alerts.Add(alert);
        }
    }

    /// <inheritdoc />
    public Task PreCallAsync(FunctionCallContent call, CancellationToken cancellationToken = default)
    {
        var argsText = JsonSerializer.Serialize(call.Arguments ?? new Dictionary<string, object?>());

        // Scan all first so every finding is recorded even when one of them blocks.
        NarcFinding? block = null;
        foreach (var f in Narc.ScanInjection(argsText))
        {
            if (block is null && f.Severity >= Severity.Block)
            {
                block = f;
            }

            Record(new NarcAlert(f.Severity, "injection", f.PatternName, f.Redacted, call.Name));
        }

        // Secrets in arguments: alert only (may be legitimate).
        foreach (var f in Narc.ScanSecrets(argsText))
        {
            Record(new NarcAlert(f.Severity, "secret", f.PatternName, f.Redacted, call.Name));
        }

        if (block is not null)
        {
            throw new InvalidOperationException($"prompt-injection pattern `{block.PatternName}` in tool arguments — blocked");
        }

        return Task.CompletedTask;
    }

    /// <inheritdoc />
    public Task PostCallAsync(FunctionCallContent call, FunctionResultContent result, CancellationToken cancellationToken = default)
    {
        var content = result.Result?.ToString() ?? string.Empty;

        // A secret in a tool result is a leak. Record a Block alert AND redact it out of
        // result.Result — the mutable seam means this rewrite is what the model/conversation
        // and every downstream consumer actually see, not just a log line.
        var secrets = Narc.ScanSecrets(content);
        foreach (var f in secrets)
        {
            Record(new NarcAlert(Severity.Block, "secret_leak", f.PatternName, f.Redacted, call.Name));
        }

        if (secrets.Count > 0)
        {
            content = Narc.RedactSecrets(content);
            result.Result = content;
        }

        // Injection in the result is detection + alert only (surveillance) — scan the
        // post-redaction content the model will actually see.
        foreach (var f in Narc.ScanInjection(content))
        {
            var severity = f.Severity > Severity.Alert ? f.Severity : Severity.Alert;
            Record(new NarcAlert(severity, "injection_output", f.PatternName, f.Redacted, call.Name));
        }

        return Task.CompletedTask;
    }
}
