using SmooAI.SmoothOperator.Core.Extensions;

namespace SmooAI.SmoothOperator.Core.Tests.Extensions;

/// <summary>Shared helpers for the live extension tests: spawn the Node echo peer directly, or write
/// an <c>echo</c> manifest into a temp global dir and load a host over it.</summary>
internal static class SepLive
{
    /// <summary>Skip a live test cleanly when Node isn't installed.</summary>
    public static string RequireNode()
    {
        var node = SepTestPaths.NodePath();
        Skip.If(node is null, "node runtime not available");
        return node!;
    }

    public static SpawnSpec PeerSpec(params (string Key, string Value)[] env)
    {
        var node = RequireNode();
        var spec = new SpawnSpec
        {
            Command = node,
            Args = new List<string> { SepTestPaths.EchoPeer },
            Cwd = SepTestPaths.SepDir,
        };
        foreach (var (k, v) in env)
        {
            spec.Env[k] = v;
        }
        return spec;
    }

    /// <summary>Write an <c>echo</c> manifest into a fresh temp global dir pointing at the Node peer,
    /// with the given peer env + optional hook timeout. Returns the temp dir (delete after use).</summary>
    public static string WriteEchoManifest((string Key, string Value)[] env, long? hookTimeoutMs = null)
    {
        var node = RequireNode();
        var tmp = Directory.CreateTempSubdirectory("sep-live").FullName;
        var extDir = Path.Combine(tmp, "echo");
        Directory.CreateDirectory(extDir);

        var envLine = env.Length == 0
            ? string.Empty
            : "env = { " + string.Join(", ", env.Select(e => $"{e.Key} = \"{e.Value}\"")) + " }\n";
        var timeoutLine = hookTimeoutMs is { } ms ? $"hook_timeout_ms = {ms}\n" : string.Empty;
        var toml =
            $"name = \"echo\"\nversion = \"0.1.0\"\n{timeoutLine}[run]\ncommand = \"{node}\"\nargs = [\"{SepTestPaths.EchoPeer.Replace("\\", "\\\\")}\"]\n{envLine}[capabilities]\ntools = true\n";
        File.WriteAllText(Path.Combine(extDir, "extension.toml"), toml);
        return tmp;
    }

    public static Task<(ExtensionHost Host, List<(string Name, string Error)> Failures)> LoadEchoHostAsync(
        string globalDir, HostDelegate @delegate, List<string>? uiCapabilities = null, bool trusted = true)
    {
        var (discovered, failures) = ExtensionDiscovery.Discover(globalDir, null);
        Assert.Empty(failures);
        return ExtensionHost.LoadAsync(
            discovered,
            new HostInfo { Name = "test-host", Version = "0.0.0" },
            new WorkspaceInfo { Root = "/ws", Trusted = trusted },
            "headless",
            uiCapabilities ?? new List<string>(),
            @delegate);
    }
}
