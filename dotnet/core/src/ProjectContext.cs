using System.Text;

namespace SmooAI.SmoothOperator.Core;

/// <summary>A parsed file reference from the <c>## File References</c> section.</summary>
/// <param name="Label">Display label from the markdown link text.</param>
/// <param name="Path">Relative file path (without fragment).</param>
/// <param name="Fragment">Optional <c>#fragment</c> pointing to a heading.</param>
/// <param name="Description">Optional description after the <c> — </c>.</param>
public sealed record FileRef(string Label, string Path, string? Fragment, string? Description);

/// <summary>
/// Project context loader — AGENTS.md (or its fallbacks) plus resolved file references.
/// The C# port of the Rust reference engine's <c>context.rs</c> (pearl th-5002c4).
///
/// <para>Smooth previously only read AGENTS.md. Many projects don't have one but DO have
/// CLAUDE.md or a SMOOTH.md or <c>.smooth/CONTEXT.md</c>. User-level facts also belong in
/// the prompt, so we walk a preference order and <b>stack</b> user-level + project-level
/// context.</para>
///
/// <para>Preference order (first hit per layer; layers stack):</para>
/// <list type="bullet">
/// <item>USER layer (read once, prepended): <c>~/.smooth/CONTEXT.md</c> →
/// <c>~/.smooth/AGENTS.md</c> → <c>~/.smooth/CLAUDE.md</c></item>
/// <item>PROJECT layer (walk up from <c>workingDir</c>, first hit wins):
/// <c>.smooth/CONTEXT.md</c> → <c>SMOOTH.md</c> → <c>AGENTS.md</c> → <c>CLAUDE.md</c></item>
/// </list>
///
/// <para>AGENTS.md / SMOOTH.md can carry file references in a <c>## File References</c>
/// section (<c>- [Label](path.md#fragment) — description</c>). Those are resolved against
/// the file's directory and appended inline. The combined string is what the host injects
/// into the agent's system prompt — like the Rust reference, this is a standalone loader,
/// <b>not</b> a hook inside the agent loop.</para>
/// </summary>
public static class ProjectContext
{
    private static readonly string[] UserContextCandidates =
    [
        System.IO.Path.Combine(".smooth", "CONTEXT.md"),
        System.IO.Path.Combine(".smooth", "AGENTS.md"),
        System.IO.Path.Combine(".smooth", "CLAUDE.md"),
    ];

    private static readonly string[] ProjectContextCandidates =
    [
        System.IO.Path.Combine(".smooth", "CONTEXT.md"),
        "SMOOTH.md",
        "AGENTS.md",
        "CLAUDE.md",
    ];

    /// <summary>
    /// Load the combined project + user context, user-level prepended, with file references
    /// in any AGENTS.md / SMOOTH.md resolved inline. <c>null</c> only when NEITHER layer
    /// found anything — a workspace with a bare CLAUDE.md and no user-level file still loads.
    /// </summary>
    public static string? Load(string workingDir)
    {
        var user = LoadUserContext();
        var project = LoadLayeredProjectContext(workingDir);

        if (user is null && project is null) return null;
        if (project is null) return $"## User context (~/.smooth)\n\n{user}";
        if (user is null) return project;
        return $"## User context (~/.smooth)\n\n{user}\n\n---\n\n{project}";
    }

    /// <summary>First non-blank hit from the user-level preference list.</summary>
    private static string? LoadUserContext()
    {
        var home = Environment.GetFolderPath(Environment.SpecialFolder.UserProfile);
        if (string.IsNullOrEmpty(home)) return null;
        foreach (var candidate in UserContextCandidates)
        {
            var raw = ReadOrNull(System.IO.Path.Combine(home, candidate));
            if (raw is not null && raw.Trim().Length > 0) return raw;
        }
        return null;
    }

    /// <summary>The project context file with its file references resolved.</summary>
    private static string? LoadLayeredProjectContext(string workingDir)
    {
        var contextPath = FindProjectContextFile(workingDir);
        if (contextPath is null) return null;
        var raw = ReadOrNull(contextPath);
        if (raw is null) return null;

        var refs = ParseFileReferences(raw);
        if (refs.Count == 0) return raw;

        var baseDir = System.IO.Path.GetDirectoryName(contextPath);
        if (baseDir is null) return null;

        var resolved = ResolveReferences(baseDir, refs);
        if (resolved.Count == 0) return raw;

        var sb = new StringBuilder(raw);
        sb.Append("\n---\n\n## Resolved File References\n\n");
        foreach (var (fileRef, content) in resolved)
        {
            sb.Append(fileRef.Description is null ? $"### {fileRef.Label}\n" : $"### {fileRef.Label} — {fileRef.Description}\n");
            sb.Append("\n```\n");
            sb.Append(content);
            if (!content.EndsWith('\n')) sb.Append('\n');
            sb.Append("```\n\n");
        }
        return sb.ToString();
    }

    /// <summary>
    /// Walk up from <paramref name="startDir"/>. Preference order at each level:
    /// <c>.smooth/CONTEXT.md</c> → <c>SMOOTH.md</c> → <c>AGENTS.md</c> → <c>CLAUDE.md</c>.
    /// First hit wins per directory, then keep walking up.
    /// </summary>
    internal static string? FindProjectContextFile(string startDir)
    {
        var dir = startDir;
        while (true)
        {
            foreach (var candidate in ProjectContextCandidates)
            {
                var path = System.IO.Path.Combine(dir, candidate);
                if (File.Exists(path)) return path;
            }
            var parent = System.IO.Path.GetDirectoryName(dir);
            if (parent is null || parent == dir) return null;
            dir = parent;
        }
    }

    /// <summary>
    /// Parse the <c>## File References</c> section out of AGENTS.md content. Expects
    /// markdown list items like <c>- [Label](path.md#fragment) — description</c>.
    /// </summary>
    public static IReadOnlyList<FileRef> ParseFileReferences(string content)
    {
        var refs = new List<FileRef>();
        var inSection = false;

        foreach (var line in SplitLines(content))
        {
            var trimmed = line.Trim();

            // Detect the file references section.
            if (trimmed.StartsWith("## ", StringComparison.Ordinal) || trimmed.StartsWith("# ", StringComparison.Ordinal))
            {
                inSection = trimmed.ToLowerInvariant().Contains("file reference", StringComparison.Ordinal);
                continue;
            }
            if (!inSection) continue;

            var parsed = ParseLinkLine(trimmed);
            if (parsed is not null) refs.Add(parsed);
        }
        return refs;
    }

    /// <summary>Parse a single markdown list-item link line.</summary>
    internal static FileRef? ParseLinkLine(string line)
    {
        // Strip leading `- ` or `* `.
        if (!line.StartsWith("- ", StringComparison.Ordinal) && !line.StartsWith("* ", StringComparison.Ordinal)) return null;
        var rest = line[2..];

        // Match [label](target).
        var openBracket = rest.IndexOf('[');
        if (openBracket < 0) return null;
        var closeBracket = rest.IndexOf(']', openBracket);
        if (closeBracket < 0) return null;
        var label = rest[(openBracket + 1)..closeBracket];

        var after = rest[(closeBracket + 1)..];
        var openParen = after.IndexOf('(');
        if (openParen < 0) return null;
        var closeParen = after.IndexOf(')', openParen);
        if (closeParen < 0) return null;
        var target = after[(openParen + 1)..closeParen];

        // Split path and fragment.
        var hash = target.IndexOf('#');
        var path = hash < 0 ? target : target[..hash];
        var fragment = hash < 0 ? null : target[(hash + 1)..];

        // Description after ` — ` / ` - ` / ` -- `.
        var afterLink = after[(closeParen + 1)..];
        string? description = null;
        foreach (var sep in new[] { " — ", " - ", " -- " })
        {
            if (!afterLink.StartsWith(sep, StringComparison.Ordinal)) continue;
            var candidate = afterLink[sep.Length..].Trim();
            if (candidate.Length > 0) description = candidate;
            break;
        }

        if (path.Length == 0 && fragment is null) return null;
        return new FileRef(label, path, fragment, description);
    }

    /// <summary>Resolve references against a base directory, skipping unreadable or empty ones.</summary>
    private static List<(FileRef Ref, string Content)> ResolveReferences(string baseDir, IReadOnlyList<FileRef> refs)
    {
        var results = new List<(FileRef, string)>();
        foreach (var fileRef in refs)
        {
            var raw = ReadOrNull(System.IO.Path.Combine(baseDir, fileRef.Path));
            if (raw is null) continue; // Skip unreadable files.
            var content = fileRef.Fragment is null ? raw : ExtractSection(raw, fileRef.Fragment);
            if (content.Trim().Length > 0) results.Add((fileRef, content));
        }
        return results;
    }

    /// <summary>
    /// Extract a markdown section by heading fragment. The fragment is matched against
    /// GitHub-style heading anchors; the section runs until the next heading at the same
    /// or a higher level.
    /// </summary>
    internal static string ExtractSection(string content, string fragment)
    {
        var target = HeadingToAnchor(fragment);
        var lines = SplitLines(content);
        var start = -1;
        var startLevel = 0;

        for (var i = 0; i < lines.Length; i++)
        {
            if (ParseHeading(lines[i]) is not (var level, var text)) continue;
            var anchor = HeadingToAnchor(text);
            if (anchor == target || anchor.Contains(target, StringComparison.Ordinal) || target.Contains(anchor, StringComparison.Ordinal))
            {
                start = i;
                startLevel = level;
                continue;
            }
            // Started capturing and hit a same-or-higher-level heading: stop.
            if (start >= 0 && level <= startLevel) return string.Join('\n', lines[start..i]);
        }

        return start >= 0 ? string.Join('\n', lines[start..]) : string.Empty;
    }

    /// <summary>Parse a markdown heading line into its level and text.</summary>
    private static (int Level, string Text)? ParseHeading(string line)
    {
        var trimmed = line.Trim();
        if (!trimmed.StartsWith('#')) return null;
        var level = 0;
        while (level < trimmed.Length && trimmed[level] == '#') level++;
        var text = trimmed[level..].Trim();
        return text.Length == 0 ? null : (level, text);
    }

    /// <summary>Convert heading text to a GitHub-style anchor.</summary>
    internal static string HeadingToAnchor(string text)
    {
        var sb = new StringBuilder(text.Length);
        foreach (var ch in text.ToLowerInvariant())
        {
            if (char.IsLetterOrDigit(ch) || ch is '-' or '_') sb.Append(ch);
            else if (ch == ' ') sb.Append('-');
            // Other characters are dropped.
        }
        // Single non-overlapping pass, matching Rust's `str::replace`.
        return sb.ToString().Replace("--", "-", StringComparison.Ordinal);
    }

    /// <summary><c>null</c> instead of throwing, for any unreadable path.</summary>
    private static string? ReadOrNull(string path)
    {
        try
        {
            return File.ReadAllText(path);
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException or ArgumentException or NotSupportedException)
        {
            return null;
        }
    }

    /// <summary>
    /// Mirrors Rust's <c>str::lines</c>: split on <c>\n</c> only, drop a trailing <c>\r</c>
    /// on each line, and emit no trailing empty line for a final newline.
    /// </summary>
    private static string[] SplitLines(string s)
    {
        if (s.Length == 0) return [];
        var body = s.EndsWith('\n') ? s[..^1] : s;
        var parts = body.Split('\n');
        for (var i = 0; i < parts.Length; i++)
        {
            if (parts[i].EndsWith('\r')) parts[i] = parts[i][..^1];
        }
        return parts;
    }
}
