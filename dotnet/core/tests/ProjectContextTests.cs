using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>Ports the Rust reference engine's <c>context.rs</c> tests one-for-one.</summary>
public class ProjectContextTests : IDisposable
{
    private readonly string _tmp = Directory.CreateTempSubdirectory("smooth-ctx-").FullName;

    public void Dispose()
    {
        try { Directory.Delete(_tmp, recursive: true); } catch (IOException) { /* best effort */ }
        GC.SuppressFinalize(this);
    }

    private string Write(string relativePath, string content)
    {
        var path = Path.Combine(_tmp, relativePath);
        Directory.CreateDirectory(Path.GetDirectoryName(path)!);
        File.WriteAllText(path, content);
        return path;
    }

    [Fact]
    public void ParseSimpleLink()
    {
        var r = ProjectContext.ParseLinkLine("- [CLAUDE.md](CLAUDE.md) — Project overview");
        Assert.NotNull(r);
        Assert.Equal("CLAUDE.md", r.Label);
        Assert.Equal("CLAUDE.md", r.Path);
        Assert.Null(r.Fragment);
        Assert.Equal("Project overview", r.Description);
    }

    [Fact]
    public void ParseLinkWithFragment()
    {
        var r = ProjectContext.ParseLinkLine("- [Pearl tracking](CLAUDE.md#6-pearl-tracking) — Pearl workflow");
        Assert.NotNull(r);
        Assert.Equal("Pearl tracking", r.Label);
        Assert.Equal("CLAUDE.md", r.Path);
        Assert.Equal("6-pearl-tracking", r.Fragment);
        Assert.Equal("Pearl workflow", r.Description);
    }

    [Fact]
    public void ParseLinkNoDescription()
    {
        var r = ProjectContext.ParseLinkLine("- [README](README.md)");
        Assert.NotNull(r);
        Assert.Equal("README", r.Label);
        Assert.Equal("README.md", r.Path);
        Assert.Null(r.Fragment);
        Assert.Null(r.Description);
    }

    [Fact]
    public void ParseFileReferencesSection()
    {
        const string content = "# Agent Instructions\n\nSome intro text.\n\n## File References\n\n"
            + "- [CLAUDE.md](CLAUDE.md) — Full file\n"
            + "- [Testing](CLAUDE.md#8-testing) — Testing reqs\n\n"
            + "## Other Section\n\n"
            + "- [not a ref](foo.md)\n";

        var refs = ProjectContext.ParseFileReferences(content);
        Assert.Equal(2, refs.Count);
        Assert.Equal("CLAUDE.md", refs[0].Path);
        Assert.Null(refs[0].Fragment);
        Assert.Equal("CLAUDE.md", refs[1].Path);
        Assert.Equal("8-testing", refs[1].Fragment);
    }

    [Fact]
    public void HeadingToAnchorBasic()
    {
        Assert.Equal("6-pearl-tracking", ProjectContext.HeadingToAnchor("6. Pearl Tracking"));
        Assert.Equal("testing--mandatory", ProjectContext.HeadingToAnchor("Testing - MANDATORY"));
        Assert.Equal("simple-heading", ProjectContext.HeadingToAnchor("Simple Heading"));
    }

    [Fact]
    public void ExtractSectionByFragment()
    {
        const string content = "# Top\n\nIntro\n\n## Section A\n\nContent A\n\n## Section B\n\nContent B\n\n### Subsection\n\nSub content\n";
        var section = ProjectContext.ExtractSection(content, "section-a");
        Assert.Contains("## Section A", section, StringComparison.Ordinal);
        Assert.Contains("Content A", section, StringComparison.Ordinal);
        Assert.DoesNotContain("Section B", section, StringComparison.Ordinal);
    }

    [Fact]
    public void ExtractSectionToEof()
    {
        var section = ProjectContext.ExtractSection("# Top\n\n## Last Section\n\nFinal content\n", "last-section");
        Assert.Contains("## Last Section", section, StringComparison.Ordinal);
        Assert.Contains("Final content", section, StringComparison.Ordinal);
    }

    [Fact]
    public void ExtractSectionNotFound()
    {
        Assert.Equal(string.Empty, ProjectContext.ExtractSection("# Top\n\n## Existing\n\nContent\n", "nonexistent"));
    }

    [Fact]
    public void LoadFromTempDir()
    {
        Write("CLAUDE.md", "# Project\n\nOverview\n\n## Testing\n\nAll tests must pass.\n\n## Deploy\n\nNever deploy locally.\n");
        Write("AGENTS.md", "# Agent Instructions\n\n## File References\n\n- [Testing](CLAUDE.md#testing) — Test reqs\n\n## Rules\n\nBe helpful.\n");

        var ctx = ProjectContext.Load(_tmp);
        Assert.NotNull(ctx);
        Assert.Contains("Agent Instructions", ctx, StringComparison.Ordinal);
        Assert.Contains("Resolved File References", ctx, StringComparison.Ordinal);
        Assert.Contains("All tests must pass", ctx, StringComparison.Ordinal);
    }

    [Fact]
    public void LoadFindsNothingReferringToAnEmptyDir()
    {
        // Mirrors the Rust test's caveat: the walk-up escapes to the filesystem root
        // and the user layer can't be mocked, so assert the practical guarantee —
        // nothing loaded refers to the (empty) temp dir.
        var ctx = ProjectContext.Load(_tmp);
        if (ctx is not null) Assert.DoesNotContain(_tmp, ctx, StringComparison.Ordinal);
    }

    [Fact]
    public void LoadPrefersSmoothContextOverClaudeMd()
    {
        Write(Path.Combine(".smooth", "CONTEXT.md"), "# Smooth context\n\nthe winner");
        Write("CLAUDE.md", "# Claude.md\n\nshould lose");

        var ctx = ProjectContext.Load(_tmp);
        Assert.NotNull(ctx);
        Assert.Contains("the winner", ctx, StringComparison.Ordinal);
        Assert.DoesNotContain("should lose", ctx, StringComparison.Ordinal);
    }

    [Fact]
    public void LoadFallsBackToClaudeMdWhenNoAgents()
    {
        Write("CLAUDE.md", "# CLAUDE.md\n\nfallback content");
        var ctx = ProjectContext.Load(_tmp);
        Assert.NotNull(ctx);
        Assert.Contains("fallback content", ctx, StringComparison.Ordinal);
    }

    [Fact]
    public void LoadPrefersSmoothMdOverClaudeMd()
    {
        Write("SMOOTH.md", "# SMOOTH.md\n\nsmooth wins");
        Write("CLAUDE.md", "# CLAUDE.md\n\nclaude loses");

        var ctx = ProjectContext.Load(_tmp);
        Assert.NotNull(ctx);
        Assert.Contains("smooth wins", ctx, StringComparison.Ordinal);
        Assert.DoesNotContain("claude loses", ctx, StringComparison.Ordinal);
    }

    [Fact]
    public void FindProjectContextWalksUp()
    {
        var nested = Path.Combine(_tmp, "a", "b", "c");
        Directory.CreateDirectory(nested);
        Write("CLAUDE.md", "# CLAUDE.md at root");

        var found = ProjectContext.FindProjectContextFile(nested);
        Assert.NotNull(found);
        Assert.EndsWith("CLAUDE.md", found, StringComparison.Ordinal);
    }

    [Fact]
    public void LoadWithoutFileReferencesReturnsRaw()
    {
        Write("AGENTS.md", "# Agent Instructions\n\nJust some text.\n");
        Assert.Equal("# Agent Instructions\n\nJust some text.\n", ProjectContext.Load(_tmp));
    }
}
