using System.Net;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// The real HTTP client, against a real local gateway.
///
/// Every assertion here is a round-trip: an <see cref="HttpListener"/> speaking the OpenAI
/// <c>/chat/completions</c> shape (JSON and SSE), driven through <see cref="GatewayChatClient"/>
/// and a live <see cref="SmoothAgent"/>. Nothing is mocked below the socket, so these cover the
/// things <see cref="MockLlmProvider"/> cannot: that the SSE framing parses, that <c>metadata</c>
/// reaches the wire (and is ABSENT when unset), and above all that the cost header is read on the
/// streaming path. The response headers survive the body being consumed just fine; the regression
/// core#102 fixed in Rust was keeping only the stream and dropping the response, leaving nothing to
/// read a header off at all.
/// </summary>
public class GatewayChatClientTests : IDisposable
{
    // $1000/1M tokens makes the local estimate large and obviously distinct from any
    // gateway-reported cost, so "which number won" is never ambiguous.
    private static readonly ModelPricing Pricing = new(PromptPerMillionTokens: 1000m, CompletionPerMillionTokens: 1000m);

    private readonly HttpListener _listener = new();
    private readonly List<JsonObject> _received = new();
    private readonly string _baseUrl;
    private Task? _serving;

    public GatewayChatClientTests()
    {
        var port = GetFreePort();
        _listener.Prefixes.Add($"http://127.0.0.1:{port}/");
        _listener.Start();
        _baseUrl = $"http://127.0.0.1:{port}/v1";
    }

    public void Dispose()
    {
        _listener.Close();
        GC.SuppressFinalize(this);
    }

    /// <summary>Every request body the gateway received, in order — for on-the-wire assertions.</summary>
    private JsonObject FirstRequest => _received[0];

    /// <summary>
    /// Serve exactly one request: JSON when the body has no <c>stream</c>, SSE when it does.
    /// <paramref name="headers"/> is where the cost header lives, and nowhere else.
    /// </summary>
    private void Serve(
        Dictionary<string, string>? headers = null,
        string text = "",
        (int Prompt, int Completion)? usage = null,
        string[]? deltas = null)
    {
        _serving = Task.Run(async () =>
        {
            var ctx = await _listener.GetContextAsync();
            using var reader = new StreamReader(ctx.Request.InputStream, Encoding.UTF8);
            var body = JsonNode.Parse(await reader.ReadToEndAsync())!.AsObject();
            lock (_received)
            {
                _received.Add(body);
            }

            foreach (var (name, value) in headers ?? new Dictionary<string, string>())
            {
                ctx.Response.AddHeader(name, value);
            }

            var usageNode = usage is { } u
                ? new JsonObject { ["prompt_tokens"] = u.Prompt, ["completion_tokens"] = u.Completion }
                : null;

            if (body["stream"]?.GetValue<bool>() == true)
            {
                ctx.Response.ContentType = "text/event-stream";
                foreach (var delta in deltas ?? [])
                {
                    await WriteSseAsync(ctx.Response, new JsonObject
                    {
                        ["choices"] = new JsonArray { new JsonObject { ["index"] = 0, ["delta"] = new JsonObject { ["content"] = delta } } },
                    });
                }
                if (usageNode is not null)
                {
                    await WriteSseAsync(ctx.Response, new JsonObject { ["choices"] = new JsonArray(), ["usage"] = usageNode });
                }
                var done = Encoding.UTF8.GetBytes("data: [DONE]\n\n");
                await ctx.Response.OutputStream.WriteAsync(done);
                ctx.Response.Close();
                return;
            }

            ctx.Response.ContentType = "application/json";
            var payload = new JsonObject
            {
                ["model"] = "m",
                ["choices"] = new JsonArray
                {
                    new JsonObject { ["index"] = 0, ["message"] = new JsonObject { ["role"] = "assistant", ["content"] = text } },
                },
            };
            if (usageNode is not null)
            {
                payload["usage"] = usageNode;
            }
            var bytes = Encoding.UTF8.GetBytes(payload.ToJsonString());
            await ctx.Response.OutputStream.WriteAsync(bytes);
            ctx.Response.Close();
        });
    }

    private static async Task WriteSseAsync(HttpListenerResponse response, JsonObject payload)
    {
        var bytes = Encoding.UTF8.GetBytes($"data: {payload.ToJsonString()}\n\n");
        await response.OutputStream.WriteAsync(bytes);
        await response.OutputStream.FlushAsync();
    }

    private GatewayChatClient Client() => new(_baseUrl, "k", "m");

    private static SmoothAgent Agent(GatewayChatClient client, IReadOnlyDictionary<string, object?>? metadata = null)
    {
        var options = new AgentOptions { Metadata = metadata };
        // Keyed by the model id the RESPONSE reports, which the client fills from the
        // gateway's `model` field.
        options.Pricing["m"] = Pricing;
        return new SmoothAgent(client, options);
    }

    private static int GetFreePort()
    {
        var l = new System.Net.Sockets.TcpListener(IPAddress.Loopback, 0);
        l.Start();
        var port = ((IPEndPoint)l.LocalEndpoint).Port;
        l.Stop();
        return port;
    }

    // ---- non-streaming ----

    [Fact]
    public async Task RoundTripsContentAndUsage()
    {
        Serve(text: "hello from the gateway", usage: (11, 7));
        using var client = Client();

        var response = await Agent(client).RunAsync("hi");
        await _serving!;

        Assert.Equal("hello from the gateway", response.Text);
        Assert.Equal(11, response.Usage.InputTokenCount);
        Assert.Equal(7, response.Usage.OutputTokenCount);
        Assert.Equal("m", FirstRequest["model"]!.GetValue<string>());
        Assert.Equal("hi", FirstRequest["messages"]!.AsArray()[^1]!["content"]!.GetValue<string>());
    }

    [Fact]
    public async Task FoldsTheGatewayCostHeaderIntoTheRunCost()
    {
        Serve(headers: new() { ["x-litellm-response-cost-margin-amount"] = "0.25" }, text: "hi", usage: (10, 5));
        using var client = Client();

        var response = await Agent(client).RunAsync("hi");
        await _serving!;

        // The gateway's number wins outright — not summed with, not shadowed by, the
        // $0.015 local estimate for these 15 tokens.
        Assert.Equal(0.25m, response.Cost.TotalCostUsd);
    }

    [Fact]
    public async Task FallsBackToLocalPricingWhenTheGatewayReportsNoCost()
    {
        Serve(text: "hi", usage: (10, 5));
        using var client = Client();

        var response = await Agent(client).RunAsync("hi");
        await _serving!;

        // Unmeasured must stay unmeasured: an estimate, never a recorded $0.
        Assert.Equal(0.015m, response.Cost.TotalCostUsd);
    }

    [Fact]
    public async Task ZeroMarginDoesNotZeroRealSpend()
    {
        Serve(
            headers: new() { ["x-litellm-response-cost-margin-amount"] = "0", ["x-litellm-response-cost-original"] = "0.5" },
            text: "hi",
            usage: (10, 5));
        using var client = Client();

        var response = await Agent(client).RunAsync("hi");
        await _serving!;

        Assert.Equal(0.5m, response.Cost.TotalCostUsd);
    }

    // ---- streaming ----

    [Fact]
    public async Task StreamsSseDeltasAndAssemblesTheText()
    {
        Serve(deltas: ["Hel", "lo ", "world"], usage: (4, 3));
        using var client = Client();

        var text = new StringBuilder();
        await foreach (var update in Agent(client).RunStreamingAsync("hi"))
        {
            text.Append(update.Text);
        }
        await _serving!;

        Assert.Equal("Hello world", text.ToString());
        Assert.True(FirstRequest["stream"]!.GetValue<bool>());
    }

    [Fact]
    public async Task ReadsTheCostHeaderBeforeTheSseBody()
    {
        Serve(headers: new() { ["x-litellm-response-cost"] = "0.75" }, deltas: ["hi"], usage: (10, 5));
        using var client = Client();

        // Asserted straight off the client, which is the seam under test here. The whole
        // point of the streaming path: keeping only the stream would leave no response to
        // read a header off at all — so a $0.75 surfacing here proves the client kept the
        // response, not that it read it early.
        var updates = new List<ChatResponseUpdate>();
        await foreach (var update in client.GetStreamingResponseAsync([new ChatMessage(ChatRole.User, "hi")]))
        {
            updates.Add(update);
        }
        await _serving!;

        var cost = updates.ToChatResponse().AdditionalProperties?["gatewayCostUsd"];
        Assert.Equal(0.75m, Assert.IsType<decimal>(cost));
    }

    [Fact]
    public async Task AStreamWithNoCostHeaderStaysUnmeasured()
    {
        Serve(deltas: ["hi"], usage: (10, 5));
        using var client = Client();

        var updates = new List<ChatResponseUpdate>();
        await foreach (var update in client.GetStreamingResponseAsync([new ChatMessage(ChatRole.User, "hi")]))
        {
            updates.Add(update);
        }
        await _serving!;

        var props = updates.ToChatResponse().AdditionalProperties;
        Assert.True(props is null || !props.ContainsKey("gatewayCostUsd"));
    }

    [Fact]
    public async Task StreamingSurfacesUsageFromTheFinalChunk()
    {
        Serve(deltas: ["hi"], usage: (10, 5));
        using var client = Client();

        var updates = new List<ChatResponseUpdate>();
        await foreach (var update in client.GetStreamingResponseAsync([new ChatMessage(ChatRole.User, "hi")]))
        {
            updates.Add(update);
        }
        await _serving!;

        var usage = updates.ToChatResponse().Usage;
        Assert.Equal(10, usage?.InputTokenCount);
        Assert.Equal(5, usage?.OutputTokenCount);
    }

    // ---- tool calls ----

    [Fact]
    public async Task ParsesToolCallsOffTheNonStreamingWire()
    {
        _serving = Task.Run(async () =>
        {
            var ctx = await _listener.GetContextAsync();
            using var reader = new StreamReader(ctx.Request.InputStream, Encoding.UTF8);
            lock (_received)
            {
                _received.Add(JsonNode.Parse(reader.ReadToEnd())!.AsObject());
            }
            ctx.Response.ContentType = "application/json";
            var payload = """
                {"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,
                "tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]}}]}
                """;
            var bytes = Encoding.UTF8.GetBytes(payload);
            await ctx.Response.OutputStream.WriteAsync(bytes);
            ctx.Response.Close();
        });
        using var client = Client();

        var response = await client.GetResponseAsync([new ChatMessage(ChatRole.User, "hi")]);
        await _serving;

        var call = Assert.Single(response.Messages.SelectMany(m => m.Contents).OfType<FunctionCallContent>());
        Assert.Equal("call_1", call.CallId);
        Assert.Equal("lookup", call.Name);
        Assert.Equal("x", call.Arguments!["q"]?.ToString());
    }

    [Fact]
    public async Task AssemblesToolCallArgumentFragmentsAcrossSseChunks()
    {
        _serving = Task.Run(async () =>
        {
            var ctx = await _listener.GetContextAsync();
            using var reader = new StreamReader(ctx.Request.InputStream, Encoding.UTF8);
            lock (_received)
            {
                _received.Add(JsonNode.Parse(reader.ReadToEnd())!.AsObject());
            }
            ctx.Response.ContentType = "text/event-stream";
            // The id + name open the call; `arguments` arrives split, which is what the
            // index-keyed accumulator exists for.
            foreach (var frame in new[]
            {
                """{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}""",
                """{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}""",
            })
            {
                var bytes = Encoding.UTF8.GetBytes($"data: {frame}\n\n");
                await ctx.Response.OutputStream.WriteAsync(bytes);
                await ctx.Response.OutputStream.FlushAsync();
            }
            var done = Encoding.UTF8.GetBytes("data: [DONE]\n\n");
            await ctx.Response.OutputStream.WriteAsync(done);
            ctx.Response.Close();
        });
        using var client = Client();

        var updates = new List<ChatResponseUpdate>();
        await foreach (var update in client.GetStreamingResponseAsync([new ChatMessage(ChatRole.User, "hi")]))
        {
            updates.Add(update);
        }
        await _serving;

        var call = Assert.Single(updates.SelectMany(u => u.Contents).OfType<FunctionCallContent>());
        Assert.Equal("call_1", call.CallId);
        Assert.Equal("lookup", call.Name);
        Assert.Equal("x", call.Arguments!["q"]?.ToString());
    }

    // ---- request metadata (core#100) ----

    [Fact]
    public async Task SendsMetadataOnTheWireWhenSet()
    {
        Serve(text: "hi");
        using var client = Client();

        await Agent(client, new Dictionary<string, object?> { ["agent_slug"] = "support" }).RunAsync("hi");
        await _serving!;

        Assert.Equal("support", FirstRequest["metadata"]!["agent_slug"]!.GetValue<string>());
    }

    [Fact]
    public async Task OmitsMetadataEntirelyWhenUnset()
    {
        Serve(text: "hi");
        using var client = Client();

        await Agent(client).RunAsync("hi");
        await _serving!;

        // Byte-identical to a client that never knew about the field.
        Assert.False(FirstRequest.ContainsKey("metadata"));
    }

    // ---- structured output (SMOODEV-1472) ----

    private static JsonElement WeatherSchema => JsonSerializer.Deserialize<JsonElement>(
        """{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}""");

    [Fact]
    public async Task SendsResponseFormatJsonSchemaOnTheWire()
    {
        Serve(text: """{"city":"Indianapolis"}""");
        using var client = Client();

        await client.GetResponseAsync(
            [new ChatMessage(ChatRole.User, "weather?")],
            new ChatOptions { ResponseFormat = ChatResponseFormat.ForJsonSchema(WeatherSchema, "weather_report") });
        await _serving!;

        var format = FirstRequest["response_format"]!;
        Assert.Equal("json_schema", format["type"]!.GetValue<string>());
        Assert.Equal("weather_report", format["json_schema"]!["name"]!.GetValue<string>());
        Assert.True(format["json_schema"]!["strict"]!.GetValue<bool>());
        Assert.Equal("object", format["json_schema"]!["schema"]!["type"]!.GetValue<string>());
    }

    [Fact]
    public async Task OmitsResponseFormatEntirelyWhenUnset()
    {
        Serve(text: "hi");
        using var client = Client();

        await client.GetResponseAsync([new ChatMessage(ChatRole.User, "hi")], new ChatOptions());
        await _serving!;

        // The parity bar: byte-identical to a client that never knew about the field.
        Assert.False(FirstRequest.ContainsKey("response_format"));
    }

    [Fact]
    public async Task UnnamedSchemaFallsBackToTheReferenceDefaultName()
    {
        Serve(text: """{"city":"Indianapolis"}""");
        using var client = Client();

        await client.GetResponseAsync(
            [new ChatMessage(ChatRole.User, "weather?")],
            new ChatOptions { ResponseFormat = ChatResponseFormat.ForJsonSchema(WeatherSchema) });
        await _serving!;

        Assert.Equal("structured_output", FirstRequest["response_format"]!["json_schema"]!["name"]!.GetValue<string>());
    }

    [Fact]
    public async Task SchemalessJsonModeSendsJsonObject()
    {
        Serve(text: """{"anything":true}""");
        using var client = Client();

        await client.GetResponseAsync(
            [new ChatMessage(ChatRole.User, "json please")],
            new ChatOptions { ResponseFormat = ChatResponseFormat.Json });
        await _serving!;

        // No schema to send, but a caller asking for JSON mode must not be silently dropped.
        Assert.Equal("json_object", FirstRequest["response_format"]!["type"]!.GetValue<string>());
        Assert.Null(FirstRequest["response_format"]!["json_schema"]);
    }

    // ---- failure ----

    [Fact]
    public async Task SurfacesANonSuccessStatusWithTheGatewayBody()
    {
        _serving = Task.Run(async () =>
        {
            var ctx = await _listener.GetContextAsync();
            ctx.Response.StatusCode = 429;
            var bytes = Encoding.UTF8.GetBytes("""{"error":"rate limited"}""");
            await ctx.Response.OutputStream.WriteAsync(bytes);
            ctx.Response.Close();
        });
        using var client = Client();

        var error = await Assert.ThrowsAsync<HttpRequestException>(
            () => client.GetResponseAsync([new ChatMessage(ChatRole.User, "hi")]));
        await _serving;

        Assert.Contains("429", error.Message, StringComparison.Ordinal);
        Assert.Contains("rate limited", error.Message, StringComparison.Ordinal);
    }
}
