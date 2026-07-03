using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core.Extensions;

/// <summary>Classifies a hook by its failure policy and default timeout.</summary>
public enum HookType
{
    ToolCall,
    UserBash,
    ToolResult,
    Input,
    BeforeAgentStart,
    Context,
    BeforeProviderRequest,
    MessageEnd,
    SessionBeforeCompact,
    SessionBeforeTree,
}

public static class HookTypeExtensions
{
    public static string AsString(this HookType h) => h switch
    {
        HookType.ToolCall => "tool_call",
        HookType.UserBash => "user_bash",
        HookType.ToolResult => "tool_result",
        HookType.Input => "input",
        HookType.BeforeAgentStart => "before_agent_start",
        HookType.Context => "context",
        HookType.BeforeProviderRequest => "before_provider_request",
        HookType.MessageEnd => "message_end",
        HookType.SessionBeforeCompact => "session_before_compact",
        HookType.SessionBeforeTree => "session_before_tree",
        _ => throw new ArgumentOutOfRangeException(nameof(h)),
    };

    public static HookType? FromName(string name) => name switch
    {
        "tool_call" => HookType.ToolCall,
        "user_bash" => HookType.UserBash,
        "tool_result" => HookType.ToolResult,
        "input" => HookType.Input,
        "before_agent_start" => HookType.BeforeAgentStart,
        "context" => HookType.Context,
        "before_provider_request" => HookType.BeforeProviderRequest,
        "message_end" => HookType.MessageEnd,
        "session_before_compact" => HookType.SessionBeforeCompact,
        "session_before_tree" => HookType.SessionBeforeTree,
        _ => null,
    };

    /// <summary>Fail-closed hooks (<c>tool_call</c>, <c>user_bash</c>) block the operation when an
    /// extension times out or crashes. Everything else fails open (proceeds).</summary>
    public static bool FailClosed(this HookType h) => h is HookType.ToolCall or HookType.UserBash;

    /// <summary>Default hook timeout: 60s for fail-closed (they gate execution), 5s for fail-open.
    /// A manifest <c>hook_timeout_ms</c> overrides this.</summary>
    public static TimeSpan DefaultTimeout(this HookType h) =>
        h.FailClosed() ? TimeSpan.FromSeconds(60) : TimeSpan.FromSeconds(5);
}

/// <summary>One extension's reply within a hook chain, as seen by the fold.</summary>
public abstract class HookStep
{
    public sealed class Replied : HookStep
    {
        public required HookOutcome Outcome { get; init; }
    }

    public sealed class Failed : HookStep { }
}

/// <summary>The folded result of a whole hook chain.</summary>
public abstract class FoldedHook
{
    public sealed class Proceed : FoldedHook
    {
        public required JsonNode Value { get; init; }
    }

    public sealed class Blocked : FoldedHook
    {
        public required string Reason { get; init; }
    }
}

/// <summary>The ext→host seam (ui / kv / exec / session / trust). The engine ships headless
/// defaults; frontends (the servers, the daemon) supply richer impls. Mirrors the Rust
/// <c>HostDelegate</c> trait; methods throw <see cref="RpcException"/> to surface an
/// <see cref="RpcError"/>.</summary>
public abstract class HostDelegate
{
    /// <summary>Answer a <c>ui/request</c>. Headless default: no UI available.</summary>
    public virtual Task<JsonNode> UiRequestAsync(string ext, JsonNode @params) =>
        throw new RpcException(SepCodes.NoUi, "no UI available (headless host)");

    /// <summary><c>exec/run</c>. Headless default: deny (no audited permission engine here).</summary>
    public virtual Task<JsonNode> ExecRunAsync(string ext, JsonNode @params) =>
        throw new RpcException(SepCodes.NotTrusted, "exec/run is not permitted on the headless host");

    /// <summary><c>kv/get</c>. Default: JSON file per extension. A missing key resolves to JSON null.</summary>
    public virtual Task<JsonNode?> KvGetAsync(string ext, string key)
    {
        var map = ExtensionHost.KvFileLoad(ext);
        return Task.FromResult(map.TryGetPropertyValue(key, out var v) ? v?.DeepClone() : null);
    }

    /// <summary><c>kv/set</c>. Default: JSON file per extension.</summary>
    public virtual Task KvSetAsync(string ext, string key, JsonNode? value)
    {
        var map = ExtensionHost.KvFileLoad(ext);
        map[key] = value?.DeepClone();
        ExtensionHost.KvFileStore(ext, map);
        return Task.CompletedTask;
    }

    /// <summary><c>session/send_message</c>. Context is pre-validated. Default: unavailable.</summary>
    public virtual Task<JsonNode> SessionSendMessageAsync(string ext, JsonNode @params) =>
        throw new RpcException(SepCodes.CapabilityDisabled, "session actions are unavailable on this host");

    public virtual Task<JsonNode> SessionSendUserMessageAsync(string ext, JsonNode @params) =>
        throw new RpcException(SepCodes.CapabilityDisabled, "session actions are unavailable on this host");

    public virtual Task<JsonNode> SessionAppendEntryAsync(string ext, JsonNode @params) =>
        throw new RpcException(SepCodes.CapabilityDisabled, "session actions are unavailable on this host");

    /// <summary>A <c>tool/update</c> progress notification streamed during an in-flight
    /// <c>tool/execute</c>. Fire-and-forget; the headless default drops it.</summary>
    public virtual void ToolUpdate(string ext, JsonNode @params) { }
}

/// <summary>The engine's headless delegate: NoUI, JSON-file kv, exec denied.</summary>
public sealed class DefaultHostDelegate : HostDelegate { }

/// <summary>
/// Orchestrates the set of loaded extensions in load order: hook chaining, non-blocking event
/// fanout, tool proxies, and the ext→host delegate seam. Mirrors the Rust <c>ExtensionHost</c>. The
/// security-critical parts (<see cref="FoldHookChain"/>, <see cref="ValidateCommandContext"/>) are
/// pure functions so they can be tested exhaustively against adversarial inputs.
/// </summary>
public sealed class ExtensionHost
{
    /// <summary>The SEP protocol version this host implements.</summary>
    public const int ProtocolVersion = 1;

    private readonly List<Loaded> _extensions;
    private readonly EpochCell _epoch;
    private readonly HostInfo _host;
    private readonly WorkspaceInfo _workspace;
    private readonly string _mode;
    private readonly List<string> _uiCapabilities;

    private ExtensionHost(List<Loaded> extensions, EpochCell epoch, HostInfo host, WorkspaceInfo workspace, string mode, List<string> uiCapabilities)
    {
        _extensions = extensions;
        _epoch = epoch;
        _host = host;
        _workspace = workspace;
        _mode = mode;
        _uiCapabilities = uiCapabilities;
    }

    /// <summary>An empty host: no extensions, every hook a passthrough. The zero-cost default when no
    /// extensions are configured.</summary>
    public static ExtensionHost Empty() => new(
        new List<Loaded>(),
        new EpochCell { Value = 1 },
        new HostInfo { Name = "smooth-operator-core", Version = "0.0.0" },
        new WorkspaceInfo { Root = string.Empty, Trusted = false },
        "headless",
        new List<string>());

    /// <summary>
    /// Load and initialize each discovered extension. Per-extension failures (spawn, handshake) are
    /// tolerated and returned alongside the host. In an untrusted workspace, project-scoped
    /// extensions are skipped.
    /// </summary>
    public static async Task<(ExtensionHost Host, List<(string Name, string Error)> Failures)> LoadAsync(
        IReadOnlyList<DiscoveredExtension> discovered,
        HostInfo host,
        WorkspaceInfo workspace,
        string mode,
        List<string> uiCapabilities,
        HostDelegate @delegate)
    {
        var extensions = new List<Loaded>();
        var failures = new List<(string, string)>();
        var epoch = new EpochCell { Value = 1 };

        foreach (var ext in discovered)
        {
            var name = ext.Manifest.Name;
            if (ext.Manifest.Disabled)
            {
                continue;
            }
            if (ext.Scope == Scope.Project && !workspace.Trusted)
            {
                continue; // skip project extensions in an untrusted workspace
            }
            try
            {
                extensions.Add(await LoadOneAsync(ext, host, workspace, mode, uiCapabilities, @delegate, epoch).ConfigureAwait(false));
            }
            catch (Exception e)
            {
                failures.Add((name, e.Message));
            }
        }

        var host2 = new ExtensionHost(extensions, epoch, host, workspace, mode, uiCapabilities);
        return (host2, failures);
    }

    private static async Task<Loaded> LoadOneAsync(
        DiscoveredExtension ext,
        HostInfo host,
        WorkspaceInfo workspace,
        string mode,
        List<string> uiCapabilities,
        HostDelegate @delegate,
        EpochCell epoch)
    {
        var spec = new SpawnSpec
        {
            Command = ext.Manifest.Run.Command,
            Args = ext.Manifest.Run.Args,
            Env = ext.Manifest.ResolvedEnv(),
            Cwd = ext.Root,
        };
        var handler = new HostInbound(ext.Manifest.Name, @delegate, epoch);
        var process = ExtensionProcess.Spawn(spec, handler);

        var init = await InitializeAsync(process, host, workspace, mode, uiCapabilities).ConfigureAwait(false);
        var subscriptions = EffectiveSubscriptions(ext.Manifest.Capabilities.Events, init.Registrations.Subscriptions);
        return new Loaded
        {
            Name = ext.Manifest.Name,
            Process = process,
            Init = init,
            Subscriptions = subscriptions,
            DeclaredEvents = ext.Manifest.Capabilities.Events,
            HookTimeout = ext.Manifest.HookTimeoutMs is { } ms ? TimeSpan.FromMilliseconds(ms) : null,
        };
    }

    private static async Task<InitializeResult> InitializeAsync(ExtensionProcess process, HostInfo host, WorkspaceInfo workspace, string mode, List<string> uiCapabilities)
    {
        var @params = new InitializeParams
        {
            ProtocolVersion = ProtocolVersion,
            Host = host,
            Workspace = workspace,
            Session = null,
            Mode = mode,
            UiCapabilities = uiCapabilities,
        };
        JsonNode raw;
        try
        {
            raw = await process.RequestAsync(SepMethods.Initialize, System.Text.Json.JsonSerializer.SerializeToNode(@params, SepJson.Options), TimeSpan.FromSeconds(10)).ConfigureAwait(false);
        }
        catch (Exception e)
        {
            throw new InvalidOperationException($"initialize: {e.Message}", e);
        }
        return raw.Deserialize<InitializeResult>(SepJson.Options) ?? throw new InvalidOperationException("bad initialize result");
    }

    /// <summary>Effective event subscriptions: what the extension asked for at handshake, clamped to
    /// what its manifest <c>[capabilities] events</c> declared. An empty declared list means "no
    /// declared filter" → trust the handshake as-is; a non-empty list is the outer bound the
    /// extension can never widen past.</summary>
    public static HashSet<string> EffectiveSubscriptions(IReadOnlyList<string> declared, IReadOnlyList<string> requested)
    {
        if (declared.Count == 0)
        {
            return new HashSet<string>(requested, StringComparer.Ordinal);
        }
        var declaredSet = new HashSet<string>(declared, StringComparer.Ordinal);
        return new HashSet<string>(requested.Where(declaredSet.Contains), StringComparer.Ordinal);
    }

    public int Count => _extensions.Count;

    public bool IsEmpty => _extensions.Count == 0;

    public IReadOnlyList<string> Names => _extensions.Select(e => e.Name).ToList();

    /// <summary>A fresh dispatch context. Session-mutating actions need <see cref="Tier.Command"/>.
    /// The token embeds the current epoch so it is invalidated across reloads.</summary>
    public Context Context(Tier tier) => new() { Token = $"epoch-{Interlocked.Read(ref _epoch.Value)}", Tier = tier };

    /// <summary>Bump the epoch, invalidating every previously minted context token. Called on reload.</summary>
    public void BumpEpoch() => Interlocked.Increment(ref _epoch.Value);

    /// <summary>True if any loaded extension subscribed to <paramref name="event"/>.</summary>
    public bool HasSubscriber(string @event) => _extensions.Any(e => e.Subscriptions.Contains(@event));

    /// <summary>Fire-and-forget event fanout to every subscribed extension. Non-blocking: a slow or
    /// dead extension never stalls the caller (its events shed on a bounded lane).</summary>
    public void DispatchEvent(string @event, JsonNode? payload)
    {
        if (_extensions.Count == 0)
        {
            return;
        }
        var ctx = Context(Tier.Event).ToNode();
        foreach (var ext in _extensions)
        {
            if (!ext.Subscriptions.Contains(@event))
            {
                continue;
            }
            ext.Process.SendEvent(@event, ctx, payload);
        }
    }

    /// <summary>Run a hook across every extension in load order, folding the chain. Each extension
    /// sees the prior extension's patch. Fail-open/closed per <see cref="HookType"/>.</summary>
    public async Task<FoldedHook> RunHookAsync(HookType hook, JsonNode input)
    {
        if (_extensions.Count == 0)
        {
            return new FoldedHook.Proceed { Value = input };
        }
        var ctx = Context(Tier.Command);
        JsonNode current = input;

        foreach (var ext in _extensions)
        {
            var @params = new JsonObject
            {
                ["hook"] = hook.AsString(),
                ["context"] = ctx.ToNode(),
                ["input"] = current.DeepClone(),
            };
            var timeout = ext.HookTimeout ?? hook.DefaultTimeout();
            HookStep step;
            try
            {
                var value = await ext.Process.RequestAsync(SepMethods.Hook, @params, timeout).ConfigureAwait(false);
                try
                {
                    var outcome = value.Deserialize<HookOutcome>(SepJson.Options)!;
                    step = new HookStep.Replied { Outcome = outcome };
                }
                catch
                {
                    step = new HookStep.Failed();
                }
            }
            catch
            {
                step = new HookStep.Failed();
            }

            switch (FoldHookChain(hook, current, new[] { step }))
            {
                case FoldedHook.Proceed p:
                    current = p.Value;
                    break;
                case FoldedHook.Blocked b:
                    return b;
            }
        }
        return new FoldedHook.Proceed { Value = current };
    }

    /// <summary>Fold a hook chain over <paramref name="input"/>, in load order. The security-critical
    /// policy: <c>continue</c> → unchanged; <c>modify</c> → replaced by the patch; <c>block</c> →
    /// veto; a failed step blocks a fail-closed hook, proceeds on a fail-open hook.</summary>
    public static FoldedHook FoldHookChain(HookType hook, JsonNode input, IReadOnlyList<HookStep> steps)
    {
        JsonNode current = input;
        foreach (var step in steps)
        {
            switch (step)
            {
                case HookStep.Replied { Outcome: HookOutcome.Continue }:
                    break;
                case HookStep.Replied { Outcome: HookOutcome.Modify m }:
                    current = m.Patch;
                    break;
                case HookStep.Replied { Outcome: HookOutcome.Block b }:
                    return new FoldedHook.Blocked { Reason = b.Reason ?? $"blocked by {hook.AsString()} hook" };
                case HookStep.Failed:
                    if (hook.FailClosed())
                    {
                        return new FoldedHook.Blocked { Reason = $"{hook.AsString()} hook failed (fail-closed)" };
                    }
                    break; // fail-open: proceed with the current value
            }
        }
        return new FoldedHook.Proceed { Value = current };
    }

    /// <summary>Convenience: run the <c>tool_call</c> hook (fail-closed) on a pending call.</summary>
    public Task<FoldedHook> RunToolCallHookAsync(string tool, JsonNode arguments) =>
        RunHookAsync(HookType.ToolCall, new JsonObject { ["tool"] = tool, ["arguments"] = arguments.DeepClone() });

    /// <summary>Run the <c>before_agent_start</c> hook on a system prompt, returning the possibly-
    /// rewritten prompt. Fail-open: a blocked/failed hook leaves the prompt unchanged.</summary>
    public async Task<string> BeforeAgentStartAsync(string systemPrompt)
    {
        if (_extensions.Count == 0)
        {
            return systemPrompt;
        }
        var folded = await RunHookAsync(HookType.BeforeAgentStart, new JsonObject { ["system_prompt"] = systemPrompt }).ConfigureAwait(false);
        return folded is FoldedHook.Proceed p
            ? p.Value["system_prompt"]?.GetValue<string>() ?? systemPrompt
            : systemPrompt;
    }

    /// <summary>Tool proxies for every eager tool every extension registered. Names are dotted
    /// <c>&lt;ext&gt;.&lt;tool&gt;</c>. Deferred tools are returned by <see cref="DeferredTools"/>.</summary>
    public IReadOnlyList<AITool> Tools() => CollectTools(false);

    /// <summary>Deferred tool proxies (register via <see cref="AgentOptions.DeferredTools"/>).</summary>
    public IReadOnlyList<AITool> DeferredTools() => CollectTools(true);

    private List<AITool> CollectTools(bool deferred)
    {
        var ctx = Context(Tier.Command);
        var @out = new List<AITool>();
        foreach (var ext in _extensions)
        {
            foreach (var reg in ext.Init.Registrations.Tools)
            {
                if (reg.Deferred != deferred)
                {
                    continue;
                }
                @out.Add(new ExtensionTool(ext.Name, reg, ext.Process, ctx));
            }
        }
        return @out;
    }

    /// <summary>Eager tool proxies for a single extension, minted at the CURRENT epoch. Callers use
    /// this after a <see cref="ReloadAsync"/> to re-register the reloaded extension's tools.</summary>
    public IReadOnlyList<AITool> ToolsFor(string extName)
    {
        var ctx = Context(Tier.Command);
        var ext = _extensions.FirstOrDefault(e => e.Name == extName);
        if (ext is null)
        {
            return Array.Empty<AITool>();
        }
        return ext.Init.Registrations.Tools
            .Where(reg => !reg.Deferred)
            .Select(reg => (AITool)new ExtensionTool(ext.Name, reg, ext.Process, ctx))
            .ToList();
    }

    /// <summary>Every registered slash-command across all extensions, paired with the owning
    /// extension name.</summary>
    public IReadOnlyList<(string Ext, CommandRegistration Command)> Commands()
    {
        var @out = new List<(string, CommandRegistration)>();
        foreach (var ext in _extensions)
        {
            foreach (var cmd in ext.Init.Registrations.Commands)
            {
                @out.Add((ext.Name, cmd));
            }
        }
        return @out;
    }

    /// <summary>Every keyboard shortcut across all extensions, paired with the owning extension name.</summary>
    public IReadOnlyList<(string Ext, ShortcutRegistration Shortcut)> Shortcuts()
    {
        var @out = new List<(string, ShortcutRegistration)>();
        foreach (var ext in _extensions)
        {
            foreach (var sc in ext.Init.Registrations.Shortcuts)
            {
                @out.Add((ext.Name, sc));
            }
        }
        return @out;
    }

    private ExtensionProcess? CommandOwner(string? extName, string command)
    {
        foreach (var ext in _extensions)
        {
            if (extName is not null && extName != ext.Name)
            {
                continue;
            }
            if (ext.Init.Registrations.Commands.Any(c => c.Name == command))
            {
                return ext.Process;
            }
        }
        return null;
    }

    /// <summary>Dispatch a registered slash-command to its owning extension with a COMMAND-tier
    /// context. Pass <paramref name="extName"/> to disambiguate a command registered by more than one
    /// extension; null picks the first match in load order.</summary>
    /// <exception cref="RpcException">-32601 if no loaded extension registered the command.</exception>
    public async Task<CommandExecuteResult> RunCommandAsync(string? extName, string command, JsonNode? arguments)
    {
        var process = CommandOwner(extName, command)
            ?? throw new RpcException(SepCodes.MethodNotFound, $"no extension registered command `{command}`");
        var @params = new JsonObject
        {
            ["command"] = command,
            ["context"] = Context(Tier.Command).ToNode(),
            ["arguments"] = arguments?.DeepClone(),
        };
        JsonNode raw;
        try
        {
            raw = await process.RequestAsync(SepMethods.CommandExecute, @params, TimeSpan.FromSeconds(120)).ConfigureAwait(false);
        }
        catch (Exception e)
        {
            throw new RpcException(SepCodes.InternalError, $"command/execute: {e.Message}");
        }
        return raw.Deserialize<CommandExecuteResult>(SepJson.Options) ?? new CommandExecuteResult();
    }

    /// <summary>Ask the extension that owns <paramref name="command"/> for argument completions given
    /// the <paramref name="partial"/> text. Returns an empty list on any failure (autocomplete is
    /// best-effort — never fail the caller's keystroke).</summary>
    public async Task<IReadOnlyList<Completion>> CompleteCommandAsync(string? extName, string command, string partial)
    {
        var process = CommandOwner(extName, command);
        if (process is null)
        {
            return Array.Empty<Completion>();
        }
        var @params = new JsonObject
        {
            ["command"] = command,
            ["context"] = Context(Tier.Command).ToNode(),
            ["partial"] = partial,
        };
        try
        {
            var raw = await process.RequestAsync(SepMethods.CommandComplete, @params, TimeSpan.FromSeconds(5)).ConfigureAwait(false);
            return raw.Deserialize<CommandCompleteResult>(SepJson.Options)?.Completions ?? (IReadOnlyList<Completion>)Array.Empty<Completion>();
        }
        catch
        {
            return Array.Empty<Completion>();
        }
    }

    /// <summary>Hot-reload a single extension by name: notify it, bump the epoch (invalidating its
    /// context tokens), respawn its subprocess, re-run <c>initialize</c>, then notify it again. The
    /// caller re-registers its tools via <see cref="ToolsFor"/>. Throws if the extension is not
    /// loaded; a respawn/re-init failure leaves the extension dead (reload is not atomic, but the
    /// epoch bump already fenced off stale contexts).</summary>
    public async Task ReloadAsync(string name)
    {
        var ext = _extensions.FirstOrDefault(e => e.Name == name)
            ?? throw new InvalidOperationException($"extension `{name}` is not loaded");

        var reloadCtx = Context(Tier.Event).ToNode();
        ext.Process.SendEvent("session_shutdown", reloadCtx, new JsonObject { ["reason"] = "reload" });

        BumpEpoch();
        ext.Process.Respawn();

        var init = await InitializeAsync(ext.Process, _host, _workspace, _mode, _uiCapabilities).ConfigureAwait(false);
        ext.Subscriptions = EffectiveSubscriptions(ext.DeclaredEvents, init.Registrations.Subscriptions);
        ext.Init = init;

        var startCtx = Context(Tier.Event).ToNode();
        ext.Process.SendEvent("session_start", startCtx, new JsonObject { ["reason"] = "reload" });
    }

    /// <summary>Gracefully shut down every extension (5s grace each, then kill).</summary>
    public async Task ShutdownAllAsync()
    {
        foreach (var ext in _extensions)
        {
            await ext.Process.ShutdownAsync(TimeSpan.FromSeconds(5)).ConfigureAwait(false);
        }
    }

    // -----------------------------------------------------------------------
    // The command-tier deadlock guard (security-critical) + kv defaults.
    // -----------------------------------------------------------------------

    internal static long? TokenEpoch(string token) =>
        token.StartsWith("epoch-", StringComparison.Ordinal) && long.TryParse(token.AsSpan(6), out var n) ? n : null;

    /// <summary>A session-mutating ext→host action is valid only when it presents a COMMAND-tier
    /// context whose epoch is still current. An event-tier context, or a stale token minted before a
    /// reload bumped the epoch, is rejected with -32003 ContextViolation.</summary>
    /// <exception cref="RpcException">The context is missing, event-tier, or stale.</exception>
    public static void ValidateCommandContext(JsonNode? @params, long currentEpoch)
    {
        var ctx = @params?["context"];
        var tier = ctx?["tier"]?.GetValue<string>();
        if (tier != "command")
        {
            throw new RpcException(SepCodes.ContextViolation, "session action requires a command-tier context");
        }
        var token = ctx?["token"]?.GetValue<string>() ?? string.Empty;
        if (TokenEpoch(token) is { } e && e == currentEpoch)
        {
            return;
        }
        throw new RpcException(SepCodes.ContextViolation, "session action presented a stale context (epoch mismatch)");
    }

    private static string? KvFilePath(string ext)
    {
        var dir = ExtensionDiscovery.DefaultGlobalDir();
        return dir is null ? null : Path.Combine(dir, ext, "state.json");
    }

    internal static JsonObject KvFileLoad(string ext)
    {
        var path = KvFilePath(ext);
        if (path is null || !File.Exists(path))
        {
            return new JsonObject();
        }
        try
        {
            return JsonNode.Parse(File.ReadAllText(path)) as JsonObject ?? new JsonObject();
        }
        catch
        {
            return new JsonObject();
        }
    }

    internal static void KvFileStore(string ext, JsonObject map)
    {
        var path = KvFilePath(ext) ?? throw new RpcException(SepCodes.InternalError, "no home dir for kv store");
        try
        {
            var parent = Path.GetDirectoryName(path);
            if (parent is not null)
            {
                Directory.CreateDirectory(parent);
            }
            File.WriteAllText(path, map.ToJsonString(new System.Text.Json.JsonSerializerOptions { WriteIndented = true }));
        }
        catch (Exception e)
        {
            throw new RpcException(SepCodes.InternalError, $"kv write: {e.Message}");
        }
    }

    /// <summary>A loaded, initialized extension. <see cref="Init"/> and <see cref="Subscriptions"/>
    /// are swapped wholesale on reload (reference assignment is atomic).</summary>
    private sealed class Loaded
    {
        public required string Name { get; init; }
        public required ExtensionProcess Process { get; init; }
        public required InitializeResult Init;
        public required HashSet<string> Subscriptions;
        public required IReadOnlyList<string> DeclaredEvents { get; init; }
        public TimeSpan? HookTimeout { get; init; }
    }

    /// <summary>The host's mutable epoch, shared with every <see cref="HostInbound"/> so a reload's
    /// bump invalidates in-flight context tokens for every extension at once.</summary>
    internal sealed class EpochCell
    {
        public long Value;
    }
}

/// <summary>Bridges the process reader's ext→host requests to the <see cref="HostDelegate"/>. Holds
/// the host's shared epoch so it can reject stale/event-tier session actions.</summary>
internal sealed class HostInbound : IInboundHandler
{
    private readonly string _ext;
    private readonly HostDelegate _delegate;
    private readonly ExtensionHost.EpochCell _epoch;

    public HostInbound(string ext, HostDelegate @delegate, ExtensionHost.EpochCell epoch)
    {
        _ext = ext;
        _delegate = @delegate;
        _epoch = epoch;
    }

    private long CurrentEpoch => System.Threading.Interlocked.Read(ref _epoch.Value);

    public async Task<JsonNode> HandleRequestAsync(string method, JsonNode? @params)
    {
        switch (method)
        {
            case SepMethods.Ping:
                return new JsonObject();
            case SepMethods.UiRequest:
                return await _delegate.UiRequestAsync(_ext, @params ?? new JsonObject()).ConfigureAwait(false);
            case SepMethods.ExecRun:
                return await _delegate.ExecRunAsync(_ext, @params ?? new JsonObject()).ConfigureAwait(false);
            case SepMethods.SessionSendMessage:
                ExtensionHost.ValidateCommandContext(@params, CurrentEpoch);
                return await _delegate.SessionSendMessageAsync(_ext, @params!).ConfigureAwait(false);
            case SepMethods.SessionSendUserMessage:
                ExtensionHost.ValidateCommandContext(@params, CurrentEpoch);
                return await _delegate.SessionSendUserMessageAsync(_ext, @params!).ConfigureAwait(false);
            case SepMethods.SessionAppendEntry:
                ExtensionHost.ValidateCommandContext(@params, CurrentEpoch);
                return await _delegate.SessionAppendEntryAsync(_ext, @params!).ConfigureAwait(false);
            case SepMethods.KvGet:
            {
                var key = @params?["key"]?.GetValue<string>() ?? string.Empty;
                return new JsonObject { ["value"] = await _delegate.KvGetAsync(_ext, key).ConfigureAwait(false) };
            }
            case SepMethods.KvSet:
            {
                var key = @params?["key"]?.GetValue<string>() ?? string.Empty;
                var value = @params?["value"];
                await _delegate.KvSetAsync(_ext, key, value?.DeepClone()).ConfigureAwait(false);
                return new JsonObject();
            }
            default:
                throw new RpcException(SepCodes.MethodNotFound, $"method not found: {method}");
        }
    }

    public void HandleNotification(string method, JsonNode? @params)
    {
        if (method == SepMethods.ToolUpdate && @params is not null)
        {
            _delegate.ToolUpdate(_ext, @params);
        }
    }
}
