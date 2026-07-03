using SmooAI.SmoothOperator.Core.Extensions;

namespace SmooAI.SmoothOperator.Core.Tests.Extensions;

/// <summary>Manifest parsing + <c>extension.toml</c> discovery (global+project merge, project-wins,
/// broken-manifest tolerance). Mirrors the Rust <c>extension::manifest</c> tests.</summary>
[Collection("SepEnv")]
public sealed class ExtensionManifestTests
{
    private const string Minimal = """
        name = "echo"
        version = "0.1.0"
        [run]
        command = "node"
        args = ["echo.mjs"]
        """;

    [Fact]
    public void ParsesMinimalManifestWithDefaults()
    {
        var m = ExtensionManifest.Parse(Minimal);
        Assert.Equal("echo", m.Name);
        Assert.Equal(1, m.Protocol);
        Assert.Equal("node", m.Run.Command);
        Assert.Equal(new[] { "echo.mjs" }, m.Run.Args);
        Assert.False(m.Disabled);
        Assert.Empty(m.Capabilities.Events);
    }

    [Fact]
    public void ParsesFullManifest()
    {
        var text = """
            name = "gate"
            version = "2.0.0"
            protocol = 1
            hook_timeout_ms = 3000
            [run]
            command = "python3"
            args = ["-m", "gate"]
            env = { TOKEN = "${env:GATE_TOKEN}", STATIC = "x" }
            [capabilities]
            events = ["turn_start", "tool_call"]
            tools = true
            ui = true
            [resources]
            skills = "skills"
            """;
        var m = ExtensionManifest.Parse(text);
        Assert.Equal(3000, m.HookTimeoutMs);
        Assert.True(m.Capabilities.Tools && m.Capabilities.Ui && !m.Capabilities.Exec);
        Assert.Equal(new[] { "turn_start", "tool_call" }, m.Capabilities.Events);
        Assert.Equal("skills", m.Resources.Skills);
        Assert.Equal("x", m.Run.Env["STATIC"]);
    }

    [Fact]
    public void MalformedManifestThrows()
    {
        Assert.ThrowsAny<Exception>(() => ExtensionManifest.Parse("name = 3\n"));
        Assert.ThrowsAny<Exception>(() => ExtensionManifest.Parse("not toml : : :"));
        Assert.ThrowsAny<Exception>(() => ExtensionManifest.Parse("name=\"x\"\nversion=\"1\"\n")); // no [run]
    }

    [Fact]
    public void ResolvedEnvExpandsEnvRefs()
    {
        Environment.SetEnvironmentVariable("SEP_TEST_TOKEN", "secret123");
        try
        {
            var text = """
                name = "e"
                version = "1"
                [run]
                command = "c"
                env = { A = "pre-${env:SEP_TEST_TOKEN}-post", B = "${env:SEP_TEST_UNSET_XYZ}" }
                """;
            var env = ExtensionManifest.Parse(text).ResolvedEnv();
            Assert.Equal("pre-secret123-post", env["A"]);
            Assert.Equal("", env["B"]); // unset → empty
        }
        finally
        {
            Environment.SetEnvironmentVariable("SEP_TEST_TOKEN", null);
        }
    }

    [Fact]
    public void ExpandEnvHandlesUnterminatedRef()
    {
        Assert.Equal("a${env:FOO", ExtensionManifest.ExpandEnv("a${env:FOO"));
        Assert.Equal("plain", ExtensionManifest.ExpandEnv("plain"));
    }

    private static void WriteExt(string dir, string name, string body)
    {
        var extDir = Path.Combine(dir, name);
        Directory.CreateDirectory(extDir);
        File.WriteAllText(Path.Combine(extDir, "extension.toml"), body);
    }

    [Fact]
    public void DiscoverMergesProjectOverGlobal()
    {
        var tmp = Directory.CreateTempSubdirectory("sep-disc");
        try
        {
            var global = Path.Combine(tmp.FullName, "global");
            var project = Path.Combine(tmp.FullName, "project");
            WriteExt(global, "echo", "name=\"echo\"\nversion=\"1.0.0\"\n[run]\ncommand=\"g\"\n");
            WriteExt(global, "only_global", "name=\"only_global\"\nversion=\"1\"\n[run]\ncommand=\"g\"\n");
            WriteExt(project, "echo", "name=\"echo\"\nversion=\"2.0.0\"\n[run]\ncommand=\"p\"\n");
            WriteExt(project, "only_project", "name=\"only_project\"\nversion=\"1\"\n[run]\ncommand=\"p\"\n");

            var (found, failures) = ExtensionDiscovery.Discover(global, project);
            Assert.Empty(failures);
            Assert.Equal(3, found.Count);

            var echo = found.First(e => e.Manifest.Name == "echo");
            Assert.Equal("2.0.0", echo.Manifest.Version); // project won
            Assert.Equal(Scope.Project, echo.Scope);
            Assert.Contains(found, e => e.Manifest.Name == "only_global" && e.Scope == Scope.Global);
            Assert.Contains(found, e => e.Manifest.Name == "only_project");
        }
        finally
        {
            tmp.Delete(recursive: true);
        }
    }

    [Fact]
    public void DiscoverToleratesOneBrokenManifest()
    {
        var tmp = Directory.CreateTempSubdirectory("sep-broken");
        try
        {
            var global = Path.Combine(tmp.FullName, "g");
            WriteExt(global, "good", "name=\"good\"\nversion=\"1\"\n[run]\ncommand=\"c\"\n");
            WriteExt(global, "bad", "this is not = = valid toml\n[[[");

            var (found, failures) = ExtensionDiscovery.Discover(global, null);
            Assert.Single(found);
            Assert.Equal("good", found[0].Manifest.Name);
            Assert.Single(failures);
            Assert.Contains("bad", failures[0].Source);
        }
        finally
        {
            tmp.Delete(recursive: true);
        }
    }

    [Fact]
    public void DiscoverMissingDirsIsEmptyNotError()
    {
        var (found, failures) = ExtensionDiscovery.Discover("/no/such/global", "/no/such/project");
        Assert.Empty(found);
        Assert.Empty(failures);
    }
}
