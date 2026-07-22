using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>Ports the Rust engine's <c>permission_grants.rs</c> tests.</summary>
public class PermissionGrantsTests
{
    [Fact]
    public void NewPinsSchemaVersionOne()
    {
        Assert.Equal(0, new PermissionGrants().SchemaVersion);
        Assert.Equal(1, PermissionGrants.New().SchemaVersion);
    }

    [Fact]
    public void HostExactAndWildcard()
    {
        var g = PermissionGrants.New();
        g.Add(GrantQuery.ForNetwork("api.example.com"));
        Assert.True(g.MatchesHost("api.example.com"));
        Assert.True(g.MatchesHost("API.EXAMPLE.COM"));
        Assert.False(g.MatchesHost("other.example.com"));

        var w = PermissionGrants.New();
        w.Add(GrantQuery.ForNetwork("*.example.com"));
        Assert.True(w.MatchesHost("api.example.com"));
        Assert.True(w.MatchesHost("example.com")); // bare apex
        Assert.False(w.MatchesHost("evil-example.com"));
    }

    [Fact]
    public void BareHostRequiresExactMatch()
    {
        var g = PermissionGrants.New();
        g.Add(GrantQuery.ForNetwork("example.com"));
        Assert.True(g.MatchesHost("example.com"));
        Assert.False(g.MatchesHost("api.example.com"));
        Assert.False(g.MatchesHost("evil-example.com"));
    }

    [Fact]
    public void ToolExactOnly()
    {
        var g = PermissionGrants.New();
        g.Add(GrantQuery.ForTool("web_search"));
        Assert.True(g.MatchesTool("web_search"));
        Assert.False(g.MatchesTool("web_search_v2"));
    }

    [Fact]
    public void BashPrefixWithTrailingSpaceGuard()
    {
        var g = PermissionGrants.New();
        g.Add(GrantQuery.ForBash("cargo "));
        Assert.True(g.MatchesBash("cargo test"));
        Assert.True(g.MatchesBash("CARGO BUILD"));
        Assert.False(g.MatchesBash("cargonaut"));
    }

    [Fact]
    public void ContainsMatchesAdd()
    {
        var g = PermissionGrants.New();
        var q = GrantQuery.ForBash("npm ");
        Assert.False(g.Contains(q));
        g.Add(q);
        Assert.True(g.Contains(q));
    }

    [Fact]
    public void MergeUnions()
    {
        var a = PermissionGrants.New();
        a.Add(GrantQuery.ForNetwork("a.example.com"));
        var b = PermissionGrants.New();
        b.Add(GrantQuery.ForTool("t"));
        b.Add(GrantQuery.ForBash("pnpm "));
        a.MergeWith(b);
        Assert.True(a.MatchesHost("a.example.com"));
        Assert.True(a.MatchesTool("t"));
        Assert.True(a.MatchesBash("pnpm i"));
    }

    [Fact]
    public void SaveLoadRoundTrip()
    {
        using var tmp = new TempDir();
        var path = Path.Combine(tmp.Path, "wonk-allow.toml");
        var g = PermissionGrants.New();
        g.Add(GrantQuery.ForNetwork("*.openai.com"));
        g.Add(GrantQuery.ForTool("web_search"));
        g.Add(GrantQuery.ForBash("cargo "));
        g.SaveToPath(path);
        var back = PermissionGrants.LoadFromPath(path);
        Assert.Equal(g.Network.AllowHosts, back.Network.AllowHosts);
        Assert.Equal(g.Tools.Allow, back.Tools.Allow);
        Assert.Equal(g.Bash.AllowPatterns, back.Bash.AllowPatterns);
    }

    [Fact]
    public void LoadMissingIsEmptyNotError()
    {
        using var tmp = new TempDir();
        var g = PermissionGrants.LoadFromPath(Path.Combine(tmp.Path, "nope.toml"));
        Assert.Equal(1, g.SchemaVersion);
        Assert.Empty(g.Network.AllowHosts);
    }

    [Fact]
    public void LoadMalformedSurfacesError()
    {
        using var tmp = new TempDir();
        var path = Path.Combine(tmp.Path, "wonk-allow.toml");
        File.WriteAllText(path, "this is [not valid = toml");
        var err = Assert.Throws<InvalidOperationException>(() => PermissionGrants.LoadFromPath(path));
        Assert.Contains("malformed wonk-allow.toml", err.Message);
    }

    [Fact]
    public void SaveIsAtomicAndCreatesDirs()
    {
        using var tmp = new TempDir();
        var path = Path.Combine(tmp.Path, "nested", "dir", "wonk-allow.toml");
        var g = PermissionGrants.New();
        g.Add(GrantQuery.ForNetwork("a.example.com"));
        g.SaveToPath(path);
        Assert.True(File.Exists(path));
        Assert.False(File.Exists(path + ".tmp"));
    }

    [Fact]
    public void AppendGrantCreatesThenExtendsIdempotently()
    {
        using var tmp = new TempDir();
        var path = Path.Combine(tmp.Path, "wonk-allow.toml");
        PermissionGrants.AppendGrant(path, GrantQuery.ForBash("npm "));
        PermissionGrants.AppendGrant(path, GrantQuery.ForBash("npm ")); // dup
        PermissionGrants.AppendGrant(path, GrantQuery.ForNetwork("api.example.com"));
        var g = PermissionGrants.LoadFromPath(path);
        Assert.Single(g.Bash.AllowPatterns);
        Assert.True(g.MatchesBash("npm install left-pad"));
        Assert.True(g.MatchesHost("api.example.com"));
    }

    [Fact]
    public void LoadLayeredProjectWinsSchemaButUnionsEntries()
    {
        using var tmp = new TempDir();
        var user = Path.Combine(tmp.Path, "user.toml");
        var project = Path.Combine(tmp.Path, "project.toml");
        var u = PermissionGrants.New();
        u.Add(GrantQuery.ForBash("cargo "));
        u.SaveToPath(user);
        var p = PermissionGrants.New();
        p.Add(GrantQuery.ForBash("pnpm "));
        p.Add(GrantQuery.ForTool("web_search"));
        p.SaveToPath(project);

        var merged = PermissionGrants.LoadLayered(user, project);
        Assert.True(merged.MatchesBash("cargo test"));
        Assert.True(merged.MatchesBash("pnpm i"));
        Assert.True(merged.MatchesTool("web_search"));
    }

    [Fact]
    public void SharedSnapshotIsIsolatedAndMergeVisible()
    {
        var shared = new SharedGrants(PermissionGrants.New());
        var more = PermissionGrants.New();
        more.Add(GrantQuery.ForNetwork("b.example.com"));
        shared.MergeIn(more);
        Assert.True(shared.Snapshot().MatchesHost("b.example.com"));
        // Mutating a snapshot does not touch the store.
        var snap = shared.Snapshot();
        snap.Add(GrantQuery.ForNetwork("c.example.com"));
        Assert.False(shared.Snapshot().MatchesHost("c.example.com"));
    }

    [Fact]
    public void ProjectGrantsPathShape() =>
        Assert.Equal(Path.Combine("/tmp/x", ".smooth", "wonk-allow.toml"), PermissionGrants.ProjectGrantsPath("/tmp/x"));

    private sealed class TempDir : IDisposable
    {
        public string Path { get; } = Directory.CreateTempSubdirectory("permgrants").FullName;

        public void Dispose()
        {
            try
            {
                Directory.Delete(Path, recursive: true);
            }
            catch
            {
                // best-effort cleanup
            }
        }
    }
}
