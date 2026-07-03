using System.Text.Json.Nodes;
using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core.Extensions;

namespace SmooAI.SmoothOperator.Core.Tests.Extensions;

/// <summary>Live end-to-end host behavior over the Node echo peer: tool proxy invoke, the
/// fail-closed/fail-open/modify hook paths, the ext→host <c>ui/request</c> seam, command dispatch +
/// completion, and the untrusted-project skip. Mirrors the Rust <c>sep_agent_integration.rs</c> /
/// <c>sep_ui_path.rs</c> / <c>sep_command_reload.rs</c> at the host seam. Skips without Node.</summary>
public sealed class ExtensionHostLiveTests
{
    /// <summary>A delegate that records ui/request calls and answers confirm with a fixed verdict.</summary>
    private sealed class RecordingUiDelegate : HostDelegate
    {
        public List<string> UiHits { get; } = new();
        private readonly bool _confirmed;
        public RecordingUiDelegate(bool confirmed) => _confirmed = confirmed;

        public override Task<JsonNode> UiRequestAsync(string ext, JsonNode @params)
        {
            UiHits.Add(@params["prompt"]?.GetValue<string>() ?? "");
            return Task.FromResult<JsonNode>(new JsonObject { ["confirmed"] = _confirmed });
        }
    }

    [SkippableFact]
    public async Task LoadsExposesAndInvokesExtensionTool()
    {
        var dir = SepLive.WriteEchoManifest(Array.Empty<(string, string)>());
        try
        {
            var (host, failures) = await SepLive.LoadEchoHostAsync(dir, new DefaultHostDelegate());
            Assert.Empty(failures);
            Assert.Equal(1, host.Count);

            var tool = Assert.IsAssignableFrom<AIFunction>(Assert.Single(host.Tools()));
            Assert.Equal("echo.say", tool.Name);

            var result = await tool.InvokeAsync(new AIFunctionArguments { ["phrase"] = "hello from the LLM" });
            Assert.Equal("hello from the LLM", result?.ToString());

            await host.ShutdownAllAsync();
        }
        finally
        {
            Directory.Delete(dir, recursive: true);
        }
    }

    [SkippableFact]
    public async Task ToolCallHookVetoesFailClosed()
    {
        var dir = SepLive.WriteEchoManifest(new[] { ("SEP_ECHO_BLOCK", "1") });
        try
        {
            var (host, _) = await SepLive.LoadEchoHostAsync(dir, new DefaultHostDelegate());
            var folded = await host.RunToolCallHookAsync("danger", new JsonObject());
            var blocked = Assert.IsType<FoldedHook.Blocked>(folded);
            Assert.Equal("blocked by echo peer", blocked.Reason);
            await host.ShutdownAllAsync();
        }
        finally
        {
            Directory.Delete(dir, recursive: true);
        }
    }

    [SkippableFact]
    public async Task ToolResultHookPatchesContentFailOpen()
    {
        var dir = SepLive.WriteEchoManifest(new[] { ("SEP_ECHO_PATCH", "1") });
        try
        {
            var (host, _) = await SepLive.LoadEchoHostAsync(dir, new DefaultHostDelegate());
            var folded = await host.RunHookAsync(HookType.ToolResult, new JsonObject { ["content"] = "raw", ["is_error"] = false });
            var proceed = Assert.IsType<FoldedHook.Proceed>(folded);
            Assert.Equal("[patched by echo]", proceed.Value["content"]!.GetValue<string>());
            await host.ShutdownAllAsync();
        }
        finally
        {
            Directory.Delete(dir, recursive: true);
        }
    }

    [SkippableFact]
    public async Task HungToolCallHookFailsClosedWithoutStalling()
    {
        var dir = SepLive.WriteEchoManifest(new[] { ("SEP_ECHO_HANG", "1") }, hookTimeoutMs: 200);
        try
        {
            var (host, _) = await SepLive.LoadEchoHostAsync(dir, new DefaultHostDelegate());
            var run = host.RunToolCallHookAsync("danger", new JsonObject());
            var completed = await Task.WhenAny(run, Task.Delay(TimeSpan.FromSeconds(10)));
            Assert.Same(run, completed); // did not stall
            var blocked = Assert.IsType<FoldedHook.Blocked>(await run);
            Assert.Contains("fail-closed", blocked.Reason);
            await host.ShutdownAllAsync();
        }
        finally
        {
            Directory.Delete(dir, recursive: true);
        }
    }

    [SkippableFact]
    public async Task UiRequestReachesDelegateAndAnswerFlowsBack()
    {
        var dir = SepLive.WriteEchoManifest(new[] { ("SEP_ECHO_UI", "1") });
        try
        {
            var @delegate = new RecordingUiDelegate(confirmed: true);
            var (host, _) = await SepLive.LoadEchoHostAsync(dir, @delegate, uiCapabilities: new List<string> { "confirm" });

            var tool = (AIFunction)host.Tools().Single();
            var result = await tool.InvokeAsync(new AIFunctionArguments { ["phrase"] = "x" });

            Assert.Equal("confirmed=true", result?.ToString());
            Assert.Single(@delegate.UiHits);
            Assert.Contains("confirm", @delegate.UiHits[0]); // ui_capabilities threaded into the prompt
            await host.ShutdownAllAsync();
        }
        finally
        {
            Directory.Delete(dir, recursive: true);
        }
    }

    [SkippableFact]
    public async Task CommandDispatchAndCompletion()
    {
        var dir = SepLive.WriteEchoManifest(Array.Empty<(string, string)>());
        try
        {
            var (host, _) = await SepLive.LoadEchoHostAsync(dir, new DefaultHostDelegate());
            var (ext, cmd) = Assert.Single(host.Commands());
            Assert.Equal("echo", ext);
            Assert.Equal("echo-cmd", cmd.Name);

            var res = await host.RunCommandAsync(null, "echo-cmd", new JsonObject { ["x"] = 1 });
            Assert.Equal("ran echo-cmd", res.Content);

            var completions = await host.CompleteCommandAsync(null, "echo-cmd", "on");
            Assert.Equal("on-done", Assert.Single(completions).Value);
            await host.ShutdownAllAsync();
        }
        finally
        {
            Directory.Delete(dir, recursive: true);
        }
    }

    [SkippableFact]
    public async Task ProjectExtensionSkippedInUntrustedWorkspace()
    {
        SepLive.RequireNode();
        var tmp = Directory.CreateTempSubdirectory("sep-untrusted").FullName;
        try
        {
            // A project-scoped extension: discovered from the project dir, so an untrusted workspace skips it.
            var projectExtRoot = SepLive.WriteEchoManifest(Array.Empty<(string, string)>());
            var (discovered, _) = ExtensionDiscovery.Discover(null, Path.Combine(projectExtRoot));
            var (host, _) = await ExtensionHost.LoadAsync(
                discovered,
                new HostInfo { Name = "t", Version = "0" },
                new WorkspaceInfo { Root = "/ws", Trusted = false },
                "headless",
                new List<string>(),
                new DefaultHostDelegate());
            Assert.True(host.IsEmpty);
            Directory.Delete(projectExtRoot, recursive: true);
        }
        finally
        {
            Directory.Delete(tmp, recursive: true);
        }
    }
}
