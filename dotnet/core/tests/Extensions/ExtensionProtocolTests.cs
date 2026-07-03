using System.Text.Json;
using System.Text.Json.Nodes;
using SmooAI.SmoothOperator.Core.Extensions;

namespace SmooAI.SmoothOperator.Core.Tests.Extensions;

/// <summary>
/// SEP conformance replay — the C# host's side of the shared fixture suite (mirrors the Rust
/// <c>sep_conformance.rs</c>). Every typed method fixture in the vendored <c>sep/fixtures.json</c>
/// must deserialize into its C# struct and re-serialize losslessly, and every <c>$invalid</c>
/// instance that violates a C#-enforced constraint must be rejected. Plus the JSON-RPC envelope
/// classification + tagged HookOutcome / Tier serialization.
/// </summary>
public sealed class ExtensionProtocolTests
{
    private static readonly JsonObject Fixtures = LoadFixtures();

    private static JsonObject LoadFixtures() =>
        JsonNode.Parse(File.ReadAllText(SepTestPaths.FixturesJson))!.AsObject();

    private static JsonNode Instance(string name) =>
        Fixtures[name]?["instance"] ?? throw new InvalidOperationException($"fixture `{name}` missing");

    private static JsonNode Invalid(string name)
    {
        foreach (var e in Fixtures["$invalid"]!.AsArray())
        {
            if (e?["name"]?.GetValue<string>() == name)
            {
                return e["instance"]!;
            }
        }
        throw new InvalidOperationException($"invalid fixture `{name}` missing");
    }

    /// <summary>Deserialize into T, re-serialize, and assert every field the fixture carries survives
    /// with an equal value (fidelity: no data loss on the wire fields).</summary>
    private static void AssertRoundtrip<T>(JsonNode fixture)
    {
        var typed = fixture.Deserialize<T>(SepJson.Options);
        Assert.NotNull(typed);
        var reserialized = JsonSerializer.SerializeToNode(typed, SepJson.Options)!;
        AssertContains(fixture, reserialized);
    }

    private static void AssertContains(JsonNode expected, JsonNode actual)
    {
        switch (expected)
        {
            case JsonObject eo:
                var ao = actual as JsonObject ?? throw new Xunit.Sdk.XunitException($"expected object, got {actual.ToJsonString()}");
                foreach (var (key, value) in eo)
                {
                    Assert.True(ao.ContainsKey(key), $"missing key `{key}` in {actual.ToJsonString()}");
                    if (value is not null)
                    {
                        AssertContains(value, ao[key]!);
                    }
                }
                break;
            case JsonArray ea:
                var aa = actual as JsonArray ?? throw new Xunit.Sdk.XunitException($"expected array, got {actual.ToJsonString()}");
                Assert.Equal(ea.Count, aa.Count);
                for (var i = 0; i < ea.Count; i++)
                {
                    AssertContains(ea[i]!, aa[i]!);
                }
                break;
            default:
                Assert.Equal(expected.ToJsonString(), actual.ToJsonString());
                break;
        }
    }

    [Fact]
    public void ExposesTheFullFixtureSet()
    {
        var count = Fixtures.Count(kv => !kv.Key.StartsWith('$'));
        Assert.True(count >= 40, $"expected the full SEP fixture set, found {count}");
    }

    [Fact]
    public void LifecycleMethodFixturesRoundtripIntoTypedStructs()
    {
        AssertRoundtrip<InitializeParams>(Instance("initialize_params"));
        AssertRoundtrip<InitializeResult>(Instance("initialize_result"));
        AssertRoundtrip<ToolExecuteParams>(Instance("tool_execute_params"));
        AssertRoundtrip<ToolExecuteResult>(Instance("tool_execute_result"));
        AssertRoundtrip<ToolExecuteResult>(Instance("tool_execute_result_with_details"));
        AssertRoundtrip<ToolUpdateParams>(Instance("tool_update_params"));
        AssertRoundtrip<ToolUpdateParams>(Instance("tool_update_params_message_only"));
        AssertRoundtrip<HookOutcome>(Instance("hook_outcome_continue"));
        AssertRoundtrip<HookOutcome>(Instance("hook_outcome_block"));
        AssertRoundtrip<HookOutcome>(Instance("hook_outcome_modify"));
        AssertRoundtrip<EventParams>(Instance("event_params"));
        AssertRoundtrip<EventParams>(Instance("event_events_lost"));
        AssertRoundtrip<CommandExecuteResult>(Instance("command_execute_result"));
        AssertRoundtrip<CommandCompleteResult>(Instance("command_complete_result"));
    }

    [Fact]
    public void EventsLostMarkerHasCountButNoSeq()
    {
        var normal = Instance("event_params").Deserialize<EventParams>(SepJson.Options)!;
        Assert.NotNull(normal.Seq);

        var lost = Instance("event_events_lost").Deserialize<EventParams>(SepJson.Options)!;
        Assert.Equal("events_lost", lost.Event);
        Assert.Null(lost.Seq);
        Assert.Equal(12, lost.Payload!["lost"]!.GetValue<int>());
    }

    [Fact]
    public void FrameFixturesParseAndClassify()
    {
        Assert.True(Instance("frame_request").Deserialize<Message>(SepJson.Options)!.IsRequest);
        Assert.True(Instance("frame_notification").Deserialize<Message>(SepJson.Options)!.IsNotification);
        var ok = Instance("frame_success_response").Deserialize<Message>(SepJson.Options)!;
        Assert.True(ok.IsResponse && ok.Result is not null);

        foreach (var name in new[] { "frame_error_response", "error_blocked", "error_cancelled", "error_context_violation" })
        {
            var err = Instance(name).Deserialize<Message>(SepJson.Options)!;
            Assert.NotNull(err.Error);
        }
    }

    [Fact]
    public void InvalidFixturesThatViolateAConstraintAreRejected()
    {
        Assert.ThrowsAny<JsonException>(() => Invalid("initialize_params_missing_protocol_version").Deserialize<InitializeParams>(SepJson.Options));
        Assert.ThrowsAny<JsonException>(() => Invalid("tool_execute_params_missing_call_id").Deserialize<ToolExecuteParams>(SepJson.Options));
        Assert.ThrowsAny<JsonException>(() => Invalid("tool_execute_result_missing_content").Deserialize<ToolExecuteResult>(SepJson.Options));
        Assert.ThrowsAny<JsonException>(() => Invalid("hook_outcome_bogus_action").Deserialize<HookOutcome>(SepJson.Options));
        Assert.ThrowsAny<JsonException>(() => Invalid("hook_outcome_modify_missing_patch").Deserialize<HookOutcome>(SepJson.Options));
    }

    [Fact]
    public void MessageClassification()
    {
        var req = Message.Request(JsonValue.Create(1), "ping", new JsonObject());
        Assert.True(req.IsRequest && !req.IsNotification && !req.IsResponse);

        var note = Message.Notification("event", new JsonObject());
        Assert.True(note.IsNotification && !note.IsRequest);

        var ok = Message.Success(JsonValue.Create(1), new JsonObject());
        Assert.True(ok.IsResponse && !ok.IsRequest);

        var err = Message.ErrorResponse(JsonValue.Create(1), new RpcError(SepCodes.Blocked, "no"));
        Assert.True(err.IsResponse);
    }

    [Fact]
    public void RequestFrameOmitsResultAndError()
    {
        var json = Message.Request(JsonValue.Create(7), "tool/execute", new JsonObject { ["x"] = 1 }).ToJson();
        Assert.DoesNotContain("result", json);
        Assert.DoesNotContain("error", json);
        Assert.Contains("\"jsonrpc\":\"2.0\"", json);
        Assert.Contains("\"method\":\"tool/execute\"", json);
    }

    [Fact]
    public void NotificationHasNoId()
    {
        var json = Message.Notification("event", new JsonObject { ["event"] = "turn_start" }).ToJson();
        Assert.DoesNotContain("\"id\"", json);
    }

    [Fact]
    public void HookOutcomeVariantsSerializeByAction()
    {
        Assert.Equal("{\"action\":\"continue\"}", JsonSerializer.Serialize<HookOutcome>(new HookOutcome.Continue(), SepJson.Options));
        Assert.Equal("{\"action\":\"block\",\"reason\":\"nope\"}", JsonSerializer.Serialize<HookOutcome>(new HookOutcome.Block { Reason = "nope" }, SepJson.Options));
        Assert.Equal("{\"action\":\"block\"}", JsonSerializer.Serialize<HookOutcome>(new HookOutcome.Block(), SepJson.Options));
        Assert.Equal("{\"action\":\"modify\",\"patch\":{\"a\":1}}", JsonSerializer.Serialize<HookOutcome>(new HookOutcome.Modify { Patch = new JsonObject { ["a"] = 1 } }, SepJson.Options));
    }

    [Fact]
    public void TierSerializesSnakeCase()
    {
        Assert.Equal("\"command\"", JsonSerializer.Serialize(Tier.Command, SepJson.Options));
        Assert.Equal("\"event\"", JsonSerializer.Serialize(Tier.Event, SepJson.Options));
    }

    [Fact]
    public void RpcErrorDisplay()
    {
        Assert.Equal("JSON-RPC error -32001: headless", new RpcError(SepCodes.NoUi, "headless").ToString());
    }
}
