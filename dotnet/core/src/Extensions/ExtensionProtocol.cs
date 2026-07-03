using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.Json.Serialization;

namespace SmooAI.SmoothOperator.Core.Extensions;

/// <summary>
/// SEP wire protocol — JSON-RPC 2.0 frames and typed method params/results. SEP (the Smooth
/// Extension Protocol) is JSON-RPC 2.0 over ndjson on an extension subprocess's stdio. The
/// canonical schemas live in the <c>smooth-operator</c> repo at <c>spec/extension/</c>; the types
/// here are the C# host's view of that wire, mirroring the Rust engine's <c>extension::protocol</c>.
/// Field names are snake_case to match the spec exactly (see <see cref="SepJson.Options"/>).
/// </summary>
public static class SepJson
{
    /// <summary>Shared serializer options: snake_case members, drop nulls off the wire (the spec's
    /// <c>additionalProperties:false</c> + Rust's <c>skip_serializing_if</c>), enums as snake_case
    /// strings, plus the tagged <see cref="HookOutcome"/> converter.</summary>
    public static readonly JsonSerializerOptions Options = Build();

    private static JsonSerializerOptions Build()
    {
        var o = new JsonSerializerOptions
        {
            PropertyNamingPolicy = JsonNamingPolicy.SnakeCaseLower,
            DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull,
        };
        o.Converters.Add(new JsonStringEnumConverter(JsonNamingPolicy.SnakeCaseLower));
        o.Converters.Add(new HookOutcomeConverter());
        return o;
    }
}

/// <summary>JSON-RPC + SEP error codes (standard range plus the SEP extensions from
/// <c>spec/extension/envelope.md</c>).</summary>
public static class SepCodes
{
    public const int ParseError = -32700;
    public const int InvalidRequest = -32600;
    public const int MethodNotFound = -32601;
    public const int InvalidParams = -32602;
    public const int InternalError = -32603;

    /// <summary>A hook or policy vetoed the operation.</summary>
    public const int Blocked = -32000;
    /// <summary><c>ui/request</c> in a headless/uncapable frontend.</summary>
    public const int NoUi = -32001;
    /// <summary>Extension acted beyond its granted trust.</summary>
    public const int NotTrusted = -32002;
    /// <summary>Command-tier action attempted from an event-tier context.</summary>
    public const int ContextViolation = -32003;
    /// <summary>Method requires a capability the handshake did not enable.</summary>
    public const int CapabilityDisabled = -32004;
    /// <summary>Request cancelled via <c>$/cancel</c>.</summary>
    public const int Cancelled = -32800;
}

/// <summary>SEP method names, centralized so the host and tests never spell one wrong.</summary>
public static class SepMethods
{
    public const string Initialize = "initialize";
    public const string Shutdown = "shutdown";
    public const string Ping = "ping";
    public const string Event = "event";
    public const string Hook = "hook";
    public const string ToolExecute = "tool/execute";
    public const string ToolUpdate = "tool/update";
    public const string CommandExecute = "command/execute";
    public const string CommandComplete = "command/complete";
    public const string Cancel = "$/cancel";
    public const string RegistryUpdate = "registry/update";
    public const string ToolsSetActive = "tools/set_active";
    public const string ExecRun = "exec/run";
    public const string UiRequest = "ui/request";
    public const string Log = "log";
    public const string BusPublish = "bus/publish";
    public const string KvGet = "kv/get";
    public const string KvSet = "kv/set";
    public const string SessionSendMessage = "session/send_message";
    public const string SessionSendUserMessage = "session/send_user_message";
    public const string SessionAppendEntry = "session/append_entry";
}

/// <summary>Canonical SEP event names the host dispatches to subscribed extensions.</summary>
public static class SepEvents
{
    public const string TurnStart = "turn_start";
    public const string TurnEnd = "turn_end";
    public const string MessageStart = "message_start";
    public const string MessageUpdate = "message_update";
    public const string MessageEnd = "message_end";
    public const string ToolExecutionStart = "tool_execution_start";
    public const string ToolExecutionUpdate = "tool_execution_update";
    public const string ToolExecutionEnd = "tool_execution_end";
    /// <summary>Delivered when the bounded observe queue shed events. Carries <c>{lost: N}</c>.</summary>
    public const string EventsLost = "events_lost";
}

/// <summary>A JSON-RPC error object.</summary>
public sealed class RpcError
{
    public required int Code { get; init; }
    public required string Message { get; init; }
    public JsonNode? Data { get; init; }

    public RpcError() { }

    [System.Diagnostics.CodeAnalysis.SetsRequiredMembers]
    public RpcError(int code, string message)
    {
        Code = code;
        Message = message;
    }

    /// <summary>An <see cref="RpcException"/> carrying this error, for throwing across the host seam.</summary>
    public RpcException ToException() => new(this);

    public override string ToString() => $"JSON-RPC error {Code}: {Message}";
}

/// <summary>Thrown by host-delegate methods to surface an <see cref="RpcError"/> to the extension.</summary>
public sealed class RpcException : Exception
{
    public RpcError Error { get; }

    public RpcException(RpcError error) : base(error.ToString()) => Error = error;

    public RpcException(int code, string message) : this(new RpcError(code, message)) { }
}

/// <summary>
/// The JSON-RPC 2.0 envelope. All four frame shapes share this type; which fields are present
/// determines the shape (request: id+method; notification: method only; success: id+result;
/// error: id+error). Nulls are dropped on the wire so a request never carries a result/error key.
/// </summary>
public sealed class Message
{
    [JsonPropertyName("jsonrpc")]
    public string JsonRpc { get; set; } = "2.0";

    /// <summary>An integer or string id (or null on a parse-error response). Kept as a
    /// <see cref="JsonNode"/> so both forms round-trip without a bespoke union.</summary>
    [JsonPropertyName("id")]
    public JsonNode? Id { get; set; }

    [JsonPropertyName("method")]
    public string? Method { get; set; }

    [JsonPropertyName("params")]
    public JsonNode? Params { get; set; }

    [JsonPropertyName("result")]
    public JsonNode? Result { get; set; }

    [JsonPropertyName("error")]
    public RpcError? Error { get; set; }

    public static Message Request(JsonNode id, string method, JsonNode? @params) =>
        new() { Id = id, Method = method, Params = @params };

    public static Message Notification(string method, JsonNode? @params) =>
        new() { Method = method, Params = @params };

    public static Message Success(JsonNode id, JsonNode? result) =>
        new() { Id = id, Result = result ?? (JsonNode)new JsonObject() };

    public static Message ErrorResponse(JsonNode? id, RpcError error) =>
        new() { Id = id, Error = error };

    /// <summary>True when this frame is a request (has both id and method).</summary>
    [JsonIgnore]
    public bool IsRequest => Id is not null && Method is not null;

    /// <summary>True when this frame is a notification (has method, no id).</summary>
    [JsonIgnore]
    public bool IsNotification => Id is null && Method is not null;

    /// <summary>True when this frame is a response (has id, no method).</summary>
    [JsonIgnore]
    public bool IsResponse => Method is null && Id is not null;

    public string ToJson() => JsonSerializer.Serialize(this, SepJson.Options);

    public static Message? TryParse(string line)
    {
        try
        {
            return JsonSerializer.Deserialize<Message>(line, SepJson.Options);
        }
        catch (JsonException)
        {
            return null;
        }
    }
}

/// <summary>Whether a dispatch may only observe (<see cref="Event"/>) or may mutate the session
/// (<see cref="Command"/>). Session-mutating ext→host actions require <see cref="Command"/>.</summary>
public enum Tier
{
    Event,
    Command,
}

/// <summary>The dispatch context carried by every host→ext event/hook/tool/command.</summary>
public sealed class Context
{
    public required string Token { get; init; }
    public required Tier Tier { get; init; }

    public JsonNode ToNode() => JsonSerializer.SerializeToNode(this, SepJson.Options)!;
}

public sealed class HostInfo
{
    public required string Name { get; init; }
    public required string Version { get; init; }
}

public sealed class WorkspaceInfo
{
    public required string Root { get; init; }
    public required bool Trusted { get; init; }
}

public sealed class SessionInfo
{
    public string? Id { get; init; }
}

public sealed class InitializeParams
{
    public required int ProtocolVersion { get; init; }
    public required HostInfo Host { get; init; }
    public required WorkspaceInfo Workspace { get; init; }
    public SessionInfo? Session { get; init; }
    public required string Mode { get; init; }
    public List<string>? UiCapabilities { get; init; }
    public JsonObject? Flags { get; init; }
    public JsonNode? CapabilitiesEnabled { get; init; }
}

public sealed class ExtensionInfo
{
    public required string Name { get; init; }
    public required string Version { get; init; }
}

public sealed class ToolRegistration
{
    public required string Name { get; init; }
    public required string Description { get; init; }
    public required JsonNode Parameters { get; init; }
    public bool Deferred { get; init; }
}

public sealed class CommandRegistration
{
    public required string Name { get; init; }
    public required string Description { get; init; }
}

/// <summary>A keyboard shortcut an extension binds to one of its commands. Frontends with a key
/// surface (the TUI) honor these; headless hosts ignore them.</summary>
public sealed class ShortcutRegistration
{
    public required string Key { get; init; }
    public required string Command { get; init; }
    public string? Description { get; init; }
}

public sealed class Registrations
{
    public List<ToolRegistration> Tools { get; init; } = new();
    public List<CommandRegistration> Commands { get; init; } = new();
    public List<string> Flags { get; init; } = new();
    public List<ShortcutRegistration> Shortcuts { get; init; } = new();
    public List<string> Subscriptions { get; init; } = new();
}

public sealed class InitializeResult
{
    public required int ProtocolVersion { get; init; }
    public required ExtensionInfo Extension { get; init; }
    public Registrations Registrations { get; init; } = new();
}

public sealed class HookParams
{
    public required string Hook { get; init; }
    public required Context Context { get; init; }
    public required JsonNode Input { get; init; }
}

/// <summary>An extension's reply to a <c>hook</c>, tagged by <c>action</c>
/// (continue/block/modify). Serialized by <see cref="HookOutcomeConverter"/>.</summary>
public abstract class HookOutcome
{
    public sealed class Continue : HookOutcome { }

    public sealed class Block : HookOutcome
    {
        public string? Reason { get; init; }
    }

    public sealed class Modify : HookOutcome
    {
        public required JsonNode Patch { get; init; }
    }
}

/// <summary>Serializes/deserializes <see cref="HookOutcome"/> tagged by an <c>action</c> field,
/// rejecting unknown actions and a <c>modify</c> without a <c>patch</c> (mirrors the Rust
/// <c>#[serde(tag = "action")]</c> enum + the invalid conformance fixtures).</summary>
public sealed class HookOutcomeConverter : JsonConverter<HookOutcome>
{
    public override HookOutcome Read(ref Utf8JsonReader reader, Type typeToConvert, JsonSerializerOptions options)
    {
        var node = JsonNode.Parse(ref reader) as JsonObject
            ?? throw new JsonException("hook outcome must be an object");
        var action = node["action"]?.GetValue<string>();
        switch (action)
        {
            case "continue":
                return new HookOutcome.Continue();
            case "block":
                return new HookOutcome.Block { Reason = node["reason"]?.GetValue<string>() };
            case "modify":
                var patch = node["patch"] ?? throw new JsonException("modify hook outcome requires a patch");
                return new HookOutcome.Modify { Patch = patch.DeepClone() };
            default:
                throw new JsonException($"unknown hook action: {action}");
        }
    }

    public override void Write(Utf8JsonWriter writer, HookOutcome value, JsonSerializerOptions options)
    {
        writer.WriteStartObject();
        switch (value)
        {
            case HookOutcome.Continue:
                writer.WriteString("action", "continue");
                break;
            case HookOutcome.Block block:
                writer.WriteString("action", "block");
                if (block.Reason is not null)
                {
                    writer.WriteString("reason", block.Reason);
                }
                break;
            case HookOutcome.Modify modify:
                writer.WriteString("action", "modify");
                writer.WritePropertyName("patch");
                modify.Patch.WriteTo(writer, options);
                break;
            default:
                throw new JsonException($"unknown hook outcome type: {value.GetType()}");
        }
        writer.WriteEndObject();
    }
}

public sealed class ToolExecuteParams
{
    public required string CallId { get; init; }
    public required string Tool { get; init; }
    public required JsonNode Arguments { get; init; }
    public required Context Context { get; init; }
}

public sealed class ToolExecuteResult
{
    public required string Content { get; init; }
    public bool IsError { get; init; }
    public JsonNode? Details { get; init; }
}

public sealed class ToolUpdateParams
{
    public required string CallId { get; init; }
    public string? Message { get; init; }
    public double? Progress { get; init; }
    public JsonNode? Details { get; init; }
}

public sealed class EventParams
{
    public required string Event { get; init; }
    /// <summary>Per-connection monotonic sequence. Absent on the out-of-band <c>events_lost</c>
    /// marker (a gap in the run is itself the loss signal).</summary>
    public long? Seq { get; init; }
    public required Context Context { get; init; }
    public JsonNode? Payload { get; init; }
}

public sealed class CommandExecuteResult
{
    public string? Content { get; init; }
}

public sealed class Completion
{
    public required string Value { get; init; }
    public string? Description { get; init; }
}

public sealed class CommandCompleteResult
{
    public List<Completion> Completions { get; init; } = new();
}
