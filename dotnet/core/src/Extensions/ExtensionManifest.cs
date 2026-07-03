using System.Text.RegularExpressions;
using Tomlyn;
using Tomlyn.Model;

namespace SmooAI.SmoothOperator.Core.Extensions;

/// <summary>Where a manifest was discovered. Project extensions only load in trusted workspaces;
/// the host uses this to apply that policy.</summary>
public enum Scope
{
    Global,
    Project,
}

/// <summary>How to launch the extension subprocess.</summary>
public sealed class RunSpec
{
    public required string Command { get; init; }
    public List<string> Args { get; init; } = new();
    /// <summary>Extra env vars; values may reference <c>${env:VAR}</c>.</summary>
    public Dictionary<string, string> Env { get; init; } = new();
}

/// <summary>Capability declarations. The <see cref="Events"/> list doubles as the host's dispatch
/// filter — an extension only receives events it names here.</summary>
public sealed class Capabilities
{
    public List<string> Events { get; init; } = new();
    public bool Tools { get; init; }
    public bool Commands { get; init; }
    public bool Ui { get; init; }
    public bool Exec { get; init; }
    public bool Kv { get; init; }
    public bool Bus { get; init; }
    public bool Session { get; init; }
}

/// <summary>Resource directories the extension contributes (skills, prompts, themes).</summary>
public sealed class Resources
{
    public string? Skills { get; init; }
    public string? Prompts { get; init; }
    public string? Themes { get; init; }
}

/// <summary>A parsed <c>extension.toml</c>.</summary>
public sealed class ExtensionManifest
{
    public required string Name { get; init; }
    public required string Version { get; init; }
    /// <summary>Highest SEP protocol version the extension declares. Defaults to 1.</summary>
    public int Protocol { get; init; } = 1;
    public required RunSpec Run { get; init; }
    public Capabilities Capabilities { get; init; } = new();
    public Resources Resources { get; init; } = new();
    /// <summary>Per-extension hook timeout override, in milliseconds.</summary>
    public long? HookTimeoutMs { get; init; }
    /// <summary>Optional: skip this extension without deleting its manifest.</summary>
    public bool Disabled { get; init; }

    private static readonly Regex EnvRef = new(@"\$\{env:([^}]*)\}", RegexOptions.Compiled);

    /// <summary>Parse a manifest from TOML text.</summary>
    /// <exception cref="InvalidOperationException">The TOML is malformed or missing required fields.</exception>
    public static ExtensionManifest Parse(string tomlText)
    {
        TomlTable table;
        try
        {
            table = Toml.ToModel(tomlText);
        }
        catch (Exception e)
        {
            throw new InvalidOperationException($"parse extension.toml: {e.Message}", e);
        }

        var name = RequireString(table, "name");
        var version = RequireString(table, "version");
        if (!table.TryGetValue("run", out var runValue) || runValue is not TomlTable runTable)
        {
            throw new InvalidOperationException("parse extension.toml: missing [run] table");
        }

        var run = new RunSpec
        {
            Command = RequireString(runTable, "command"),
            Args = StringList(runTable, "args"),
            Env = StringMap(runTable, "env"),
        };

        var caps = table.TryGetValue("capabilities", out var capsValue) ? capsValue as TomlTable : null;
        var res = table.TryGetValue("resources", out var resValue) ? resValue as TomlTable : null;

        return new ExtensionManifest
        {
            Name = name,
            Version = version,
            Protocol = (int)LongOr(table, "protocol", 1),
            Run = run,
            Disabled = BoolOr(table, "disabled", false),
            HookTimeoutMs = table.ContainsKey("hook_timeout_ms") ? LongOr(table, "hook_timeout_ms", 0) : null,
            Capabilities = caps is null
                ? new Capabilities()
                : new Capabilities
                {
                    Events = StringList(caps, "events"),
                    Tools = BoolOr(caps, "tools", false),
                    Commands = BoolOr(caps, "commands", false),
                    Ui = BoolOr(caps, "ui", false),
                    Exec = BoolOr(caps, "exec", false),
                    Kv = BoolOr(caps, "kv", false),
                    Bus = BoolOr(caps, "bus", false),
                    Session = BoolOr(caps, "session", false),
                },
            Resources = res is null
                ? new Resources()
                : new Resources
                {
                    Skills = OptString(res, "skills"),
                    Prompts = OptString(res, "prompts"),
                    Themes = OptString(res, "themes"),
                },
        };
    }

    /// <summary>Load a manifest from <c>&lt;dir&gt;/extension.toml</c>.</summary>
    public static ExtensionManifest LoadDir(string dir)
    {
        var path = Path.Combine(dir, "extension.toml");
        string text;
        try
        {
            text = File.ReadAllText(path);
        }
        catch (Exception e)
        {
            throw new InvalidOperationException($"read {path}: {e.Message}", e);
        }
        return Parse(text);
    }

    /// <summary>The <c>[run] env</c> map with <c>${env:VAR}</c> references expanded against the host's
    /// current environment. Unset variables expand to empty strings.</summary>
    public Dictionary<string, string> ResolvedEnv() =>
        Run.Env.ToDictionary(kv => kv.Key, kv => ExpandEnv(kv.Value));

    internal static string ExpandEnv(string input) =>
        EnvRef.Replace(input, m => Environment.GetEnvironmentVariable(m.Groups[1].Value) ?? string.Empty);

    private static string? OptString(TomlTable t, string key) =>
        t.TryGetValue(key, out var v) && v is string s ? s : null;

    private static string RequireString(TomlTable t, string key) =>
        t.TryGetValue(key, out var v) && v is string s
            ? s
            : throw new InvalidOperationException($"parse extension.toml: `{key}` must be a string");

    private static long LongOr(TomlTable t, string key, long fallback) =>
        t.TryGetValue(key, out var v) && v is long l ? l : fallback;

    private static bool BoolOr(TomlTable t, string key, bool fallback) =>
        t.TryGetValue(key, out var v) && v is bool b ? b : fallback;

    private static List<string> StringList(TomlTable t, string key) =>
        t.TryGetValue(key, out var v) && v is TomlArray arr
            ? arr.OfType<string>().ToList()
            : new List<string>();

    private static Dictionary<string, string> StringMap(TomlTable t, string key)
    {
        if (t.TryGetValue(key, out var v) && v is TomlTable inner)
        {
            var map = new Dictionary<string, string>();
            foreach (var kv in inner)
            {
                if (kv.Value is string s)
                {
                    map[kv.Key] = s;
                }
            }
            return map;
        }
        return new Dictionary<string, string>();
    }
}

/// <summary>A discovered extension: its manifest, the directory it was found in (relative resources
/// and <c>args</c> resolve against this root), and its scope.</summary>
public sealed record DiscoveredExtension(ExtensionManifest Manifest, string Root, Scope Scope);

/// <summary><c>extension.toml</c> discovery: global + project directories, merged by name with
/// project winning. Mirrors the Rust <c>extension::manifest</c> module.</summary>
public static class ExtensionDiscovery
{
    /// <summary>Default global extensions dir: <c>$SMOOTH_HOME/extensions</c> if set, else
    /// <c>~/.smooth/extensions</c>.</summary>
    public static string? DefaultGlobalDir()
    {
        var home = Environment.GetEnvironmentVariable("SMOOTH_HOME");
        if (!string.IsNullOrEmpty(home))
        {
            return Path.Combine(home, "extensions");
        }
        var userHome = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
        return string.IsNullOrEmpty(userHome) ? null : Path.Combine(userHome, ".smooth", "extensions");
    }

    /// <summary>The project extensions directory for a workspace root.</summary>
    public static string ProjectDir(string workspaceRoot) =>
        Path.Combine(workspaceRoot, ".smooth", "extensions");

    /// <summary>
    /// Discover every extension under <paramref name="globalDir"/> and <paramref name="projectDir"/>,
    /// merging by name with <b>project winning</b>. Either directory may be null or missing (treated
    /// as empty). Returns the chosen extensions plus a list of <c>(source, error)</c> for manifests
    /// that failed to parse — a single bad manifest never aborts discovery.
    /// </summary>
    public static (List<DiscoveredExtension> Found, List<(string Source, string Error)> Failures) Discover(string? globalDir, string? projectDir)
    {
        var failures = new List<(string, string)>();
        var byName = new Dictionary<string, DiscoveredExtension>(StringComparer.Ordinal);

        // Global first, then project, so project overwrites on a name collision.
        foreach (var (dir, scope) in new[] { (globalDir, Scope.Global), (projectDir, Scope.Project) })
        {
            if (dir is null)
            {
                continue;
            }
            foreach (var found in ScanDir(dir, scope, failures))
            {
                byName[found.Manifest.Name] = found;
            }
        }

        // Stable order so load-order-dependent hook chaining is deterministic.
        var chosen = byName.Values.OrderBy(e => e.Manifest.Name, StringComparer.Ordinal).ToList();
        return (chosen, failures);
    }

    private static IEnumerable<DiscoveredExtension> ScanDir(string dir, Scope scope, List<(string, string)> failures)
    {
        if (!Directory.Exists(dir))
        {
            yield break;
        }
        foreach (var root in Directory.EnumerateDirectories(dir))
        {
            if (!File.Exists(Path.Combine(root, "extension.toml")))
            {
                continue;
            }
            DiscoveredExtension? ext = null;
            try
            {
                ext = new DiscoveredExtension(ExtensionManifest.LoadDir(root), root, scope);
            }
            catch (Exception e)
            {
                failures.Add((root, e.Message));
            }
            if (ext is not null)
            {
                yield return ext;
            }
        }
    }
}
