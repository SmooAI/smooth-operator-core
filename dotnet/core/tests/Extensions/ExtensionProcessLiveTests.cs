using System.Text.Json;
using System.Text.Json.Nodes;
using SmooAI.SmoothOperator.Core.Extensions;

namespace SmooAI.SmoothOperator.Core.Tests.Extensions;

/// <summary>Live subprocess lifecycle over the Node echo peer: handshake, ping, tool round-trip,
/// timeout+$/cancel, respawn/generation guard, graceful shutdown. Mirrors the Rust
/// <c>tests/sep_process.rs</c>. Skips cleanly when Node isn't installed.</summary>
public sealed class ExtensionProcessLiveTests
{
    private static async Task<InitializeResult> HandshakeAsync(ExtensionProcess p)
    {
        var @params = new InitializeParams
        {
            ProtocolVersion = ExtensionHost.ProtocolVersion,
            Host = new HostInfo { Name = "t", Version = "0" },
            Workspace = new WorkspaceInfo { Root = "/ws", Trusted = true },
            Mode = "headless",
        };
        var raw = await p.RequestAsync(SepMethods.Initialize, System.Text.Json.JsonSerializer.SerializeToNode(@params, SepJson.Options), TimeSpan.FromSeconds(10));
        return raw.Deserialize<InitializeResult>(SepJson.Options)!;
    }

    [SkippableFact]
    public async Task HandshakePingAndToolExecute()
    {
        var spec = SepLive.PeerSpec();
        using var p = ExtensionProcess.Spawn(spec, new DefaultInboundHandler());
        try
        {
            var init = await HandshakeAsync(p);
            Assert.Equal("echo", init.Extension.Name);
            Assert.Contains(init.Registrations.Tools, t => t.Name == "say");

            Assert.True(await p.PingHealthAsync(TimeSpan.FromSeconds(5)));

            var toolParams = new JsonObject
            {
                ["call_id"] = "c1",
                ["tool"] = "say",
                ["arguments"] = new JsonObject { ["phrase"] = "hi there" },
                ["context"] = new JsonObject { ["token"] = "epoch-1", ["tier"] = "command" },
            };
            var raw = await p.RequestAsync(SepMethods.ToolExecute, toolParams, TimeSpan.FromSeconds(5));
            Assert.Equal("hi there", raw.Deserialize<ToolExecuteResult>(SepJson.Options)!.Content);
        }
        finally
        {
            await p.ShutdownAsync(TimeSpan.FromSeconds(2));
        }
    }

    [SkippableFact]
    public async Task HungHookTimesOutAndCancels()
    {
        var spec = SepLive.PeerSpec(("SEP_ECHO_HANG", "1"));
        using var p = ExtensionProcess.Spawn(spec, new DefaultInboundHandler());
        await HandshakeAsync(p);

        var hookParams = new JsonObject
        {
            ["hook"] = "tool_call",
            ["context"] = new JsonObject { ["token"] = "epoch-1", ["tier"] = "command" },
            ["input"] = new JsonObject { ["tool"] = "bash" },
        };
        await Assert.ThrowsAsync<TimeoutException>(() => p.RequestAsync(SepMethods.Hook, hookParams, TimeSpan.FromMilliseconds(200)));
    }

    [SkippableFact]
    public async Task RespawnBumpsGenerationAndStaysUsable()
    {
        var spec = SepLive.PeerSpec();
        using var p = ExtensionProcess.Spawn(spec, new DefaultInboundHandler());
        await HandshakeAsync(p);
        var gen0 = p.Generation;

        p.Respawn();
        Assert.Equal(gen0 + 1, p.Generation);
        Assert.True(p.IsAlive);

        // Fresh child answers a new handshake.
        var init = await HandshakeAsync(p);
        Assert.Equal("echo", init.Extension.Name);
        await p.ShutdownAsync(TimeSpan.FromSeconds(2));
    }

    [SkippableFact]
    public async Task ShutdownLeavesProcessNotAlive()
    {
        var spec = SepLive.PeerSpec();
        var p = ExtensionProcess.Spawn(spec, new DefaultInboundHandler());
        await HandshakeAsync(p);
        await p.ShutdownAsync(TimeSpan.FromSeconds(2));
        Assert.False(p.IsAlive);
        p.Dispose();
    }
}
