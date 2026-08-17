using System.Net.Http.Headers;
using System.Runtime.CompilerServices;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// A REAL OpenAI-compatible <see cref="IChatClient"/> over HTTP — the .NET analogue of the Go
/// engine's <c>GatewayClient</c> (<c>go/core/openai.go</c>). Points at any
/// <c>/chat/completions</c> endpoint (the SmooAI gateway, or anything OpenAI-compatible) with a
/// base URL + Bearer key.
///
/// <para>The sibling TypeScript and Python engines wrap the official <c>openai</c> SDK, which they
/// already depend on. .NET has no such dependency here — the shipped library references only
/// <c>Microsoft.Extensions.AI.Abstractions</c> — and the MEAI OpenAI adapter would not help
/// anyway: it hides the HTTP response, and the gateway reports per-request cost ONLY in a
/// response header. So this is a full port rather than an adapter, and it stays dependency-free
/// (<c>HttpClient</c> + <c>System.Text.Json</c> are in the framework).</para>
///
/// <para>Two details are load-bearing:</para>
/// <list type="bullet">
/// <item><description><b>Streaming keeps hold of the response.</b>
/// <see cref="HttpResponseMessage.Headers"/> stays readable after the body is consumed, so the
/// cost is never racing the stream — the hazard core#102 fixed in Rust is dropping the response
/// and keeping only a stream, which leaves nothing to read a header off at all.
/// <see cref="HttpCompletionOption.ResponseHeadersRead"/> is required here for a different reason:
/// the default buffers the WHOLE body before returning, which never completes on an open SSE
/// stream. The cost rides a LEADING update (matching Go's first-chunk <c>ChatChunk{CostUSD}</c>)
/// so it survives a stream that errors before any usage arrives.</description></item>
/// <item><description><b><c>metadata</c> is omitted when unset</b> (core#100), keeping the request
/// byte-identical to a client that never knew about the field.</description></item>
/// </list>
///
/// <para><see cref="MockLlmProvider"/> remains the default/test seam; this is opt-in and nothing
/// constructs it for you.</para>
/// </summary>
public sealed class GatewayChatClient : IChatClient
{
    private static readonly JsonSerializerOptions JsonOptions = new(JsonSerializerDefaults.Web);

    private readonly HttpClient _http;
    private readonly bool _ownsHttpClient;
    private readonly Uri _endpoint;
    private readonly string _model;

    /// <summary>
    /// Build a client for <paramref name="baseUrl"/> (e.g. <c>https://llm.smoo.ai/v1</c>) using
    /// <paramref name="apiKey"/> as the Bearer token. <paramref name="model"/> is the default model
    /// id; an individual call can override it via <see cref="ChatOptions.ModelId"/>. Pass
    /// <paramref name="httpClient"/> to bring your own (proxy, handler, timeout) — one supplied
    /// this way is NOT disposed by <see cref="Dispose"/>.
    /// </summary>
    public GatewayChatClient(string baseUrl, string apiKey, string model, HttpClient? httpClient = null)
    {
        ArgumentException.ThrowIfNullOrWhiteSpace(baseUrl);
        ArgumentException.ThrowIfNullOrWhiteSpace(apiKey);
        ArgumentException.ThrowIfNullOrWhiteSpace(model);

        _endpoint = new Uri(baseUrl.TrimEnd('/') + "/chat/completions");
        _model = model;
        _ownsHttpClient = httpClient is null;
        _http = httpClient ?? new HttpClient { Timeout = TimeSpan.FromSeconds(60) };
        _http.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("Bearer", apiKey);
    }

    /// <inheritdoc />
    public async Task<ChatResponse> GetResponseAsync(
        IEnumerable<ChatMessage> messages,
        ChatOptions? options = null,
        CancellationToken cancellationToken = default)
    {
        using var request = BuildRequest(messages, options, stream: false);
        using var response = await _http.SendAsync(request, cancellationToken).ConfigureAwait(false);
        await ThrowIfNotSuccessAsync(response, cancellationToken).ConfigureAwait(false);

        var cost = GatewayCost.Parse(response.Headers);
        var payload = await response.Content.ReadAsStringAsync(cancellationToken).ConfigureAwait(false);
        var root = JsonNode.Parse(payload)?.AsObject()
            ?? throw new InvalidOperationException("Gateway returned a non-object response body.");

        var choice = root["choices"]?.AsArray().FirstOrDefault()?["message"]?.AsObject()
            ?? throw new InvalidOperationException("Gateway returned no choices.");

        var contents = new List<AIContent>();
        var text = choice["content"]?.GetValue<string?>();
        if (!string.IsNullOrEmpty(text))
        {
            contents.Add(new TextContent(text));
        }
        foreach (var call in choice["tool_calls"]?.AsArray() ?? new JsonArray())
        {
            var fn = call?["function"]?.AsObject();
            if (fn is null)
            {
                continue;
            }
            contents.Add(new FunctionCallContent(
                call!["id"]?.GetValue<string>() ?? string.Empty,
                fn["name"]?.GetValue<string>() ?? string.Empty,
                ParseArguments(fn["arguments"]?.GetValue<string>())));
        }

        var result = new ChatResponse(new ChatMessage(ChatRole.Assistant, contents))
        {
            ModelId = root["model"]?.GetValue<string>() ?? options?.ModelId ?? _model,
            Usage = ReadUsage(root["usage"]?.AsObject()),
        };
        if (cost is { } measured)
        {
            // The documented seam SmoothAgent.ResponseGatewayCost reads. Absent when the
            // gateway measured nothing, so the agent falls back to local pricing rather
            // than recording a bogus $0.
            result.AdditionalProperties = new AdditionalPropertiesDictionary { ["gatewayCostUsd"] = measured };
        }
        return result;
    }

    /// <inheritdoc />
    public async IAsyncEnumerable<ChatResponseUpdate> GetStreamingResponseAsync(
        IEnumerable<ChatMessage> messages,
        ChatOptions? options = null,
        [EnumeratorCancellation] CancellationToken cancellationToken = default)
    {
        using var request = BuildRequest(messages, options, stream: true);
        using var response = await _http
            .SendAsync(request, HttpCompletionOption.ResponseHeadersRead, cancellationToken)
            .ConfigureAwait(false);
        await ThrowIfNotSuccessAsync(response, cancellationToken).ConfigureAwait(false);

        // `response` is held for the whole method, which is what actually matters: its Headers
        // survive the body being read, so this is safe anywhere below too. The regression
        // core#102 fixed in Rust was keeping only the stream and losing the response entirely.
        var cost = GatewayCost.Parse(response.Headers);
        if (cost is { } measured)
        {
            yield return new ChatResponseUpdate
            {
                Role = ChatRole.Assistant,
                AdditionalProperties = new AdditionalPropertiesDictionary { ["gatewayCostUsd"] = measured },
            };
        }

        // Tool-call arguments arrive as fragments keyed by index; a FunctionCallContent needs the
        // WHOLE argument object, so they are accumulated here and emitted once the stream ends.
        var partials = new SortedDictionary<int, (string Id, string Name, StringBuilder Args)>();

        using var body = await response.Content.ReadAsStreamAsync(cancellationToken).ConfigureAwait(false);
        using var reader = new StreamReader(body, Encoding.UTF8);
        while (await reader.ReadLineAsync(cancellationToken).ConfigureAwait(false) is { } line)
        {
            var trimmed = line.AsSpan().Trim();
            if (!trimmed.StartsWith("data:", StringComparison.Ordinal))
            {
                continue;
            }
            var data = trimmed[5..].Trim().ToString();
            if (data == "[DONE]")
            {
                break;
            }

            JsonObject? chunk;
            try
            {
                chunk = JsonNode.Parse(data)?.AsObject();
            }
            catch (JsonException)
            {
                continue; // keep-alive / malformed payload — skip, as Go does
            }
            if (chunk is null)
            {
                continue;
            }

            if (ReadUsage(chunk["usage"]?.AsObject()) is { } usage)
            {
                yield return new ChatResponseUpdate { Role = ChatRole.Assistant, Contents = [new UsageContent(usage)] };
            }

            var delta = chunk["choices"]?.AsArray().FirstOrDefault()?["delta"]?.AsObject();
            if (delta is null)
            {
                continue;
            }

            var text = delta["content"]?.GetValue<string?>();
            if (!string.IsNullOrEmpty(text))
            {
                yield return new ChatResponseUpdate(ChatRole.Assistant, text);
            }

            foreach (var call in delta["tool_calls"]?.AsArray() ?? new JsonArray())
            {
                var index = call?["index"]?.GetValue<int>() ?? 0;
                var current = partials.TryGetValue(index, out var existing)
                    ? existing
                    : (Id: string.Empty, Name: string.Empty, Args: new StringBuilder());
                if (call?["id"]?.GetValue<string>() is { Length: > 0 } id)
                {
                    current.Id = id;
                }
                var fn = call?["function"]?.AsObject();
                if (fn?["name"]?.GetValue<string>() is { Length: > 0 } name)
                {
                    current.Name = name;
                }
                if (fn?["arguments"]?.GetValue<string>() is { Length: > 0 } fragment)
                {
                    current.Args.Append(fragment);
                }
                partials[index] = current;
            }
        }

        foreach (var (_, partial) in partials)
        {
            yield return new ChatResponseUpdate
            {
                Role = ChatRole.Assistant,
                Contents = [new FunctionCallContent(partial.Id, partial.Name, ParseArguments(partial.Args.ToString()))],
            };
        }
    }

    /// <inheritdoc />
    public object? GetService(Type serviceType, object? serviceKey = null) =>
        serviceKey is null && serviceType?.IsInstanceOfType(this) is true ? this : null;

    /// <inheritdoc />
    public void Dispose()
    {
        if (_ownsHttpClient)
        {
            _http.Dispose();
        }
    }

    // ── request building ────────────────────────────────────────────────────────────────────

    private HttpRequestMessage BuildRequest(IEnumerable<ChatMessage> messages, ChatOptions? options, bool stream)
    {
        var body = new JsonObject
        {
            ["model"] = options?.ModelId ?? _model,
            ["messages"] = BuildMessages(messages),
        };
        if (BuildTools(options) is { Count: > 0 } tools)
        {
            body["tools"] = tools;
        }
        if (options?.Temperature is { } temperature)
        {
            body["temperature"] = temperature;
        }
        if (options?.MaxOutputTokens is { } maxTokens)
        {
            body["max_tokens"] = maxTokens;
        }
        if (stream)
        {
            body["stream"] = true;
        }
        // Top-level OpenAI-compat `metadata` (LiteLLM records it on spend logs). Omitted
        // entirely when unset so the wire is byte-identical to a client without the field
        // (Rust parity: with_metadata filters empty maps to None).
        if (options?.AdditionalProperties?.TryGetValue("metadata", out var metadata) is true && metadata is not null)
        {
            body["metadata"] = JsonSerializer.SerializeToNode(metadata, JsonOptions);
        }
        // Structured output (SMOODEV-1472). MEAI already models this as
        // ChatOptions.ResponseFormat, so there is no parallel ResponseFormat type here —
        // this maps the platform's own ChatResponseFormatJson onto the OpenAI-compatible
        // wire object. Omitted entirely when unset, so the wire stays byte-identical
        // (Rust parity: the field is an Option, skipped when None).
        if (BuildResponseFormat(options?.ResponseFormat) is { } responseFormat)
        {
            body["response_format"] = responseFormat;
        }
        // Anthropic prompt-cache markers, gated on the model + route. A no-op for every
        // other upstream, so the body stays byte-identical on the OpenAI/Gemini/Groq paths.
        if (CacheControl.SupportsAnthropicCacheControl((string?)body["model"], _endpoint.ToString()))
        {
            CacheControl.Apply(body);
        }

        return new HttpRequestMessage(HttpMethod.Post, _endpoint)
        {
            Content = new StringContent(body.ToJsonString(), Encoding.UTF8, "application/json"),
            Headers = { Accept = { new MediaTypeWithQualityHeaderValue(stream ? "text/event-stream" : "application/json") } },
        };
    }

    /// <summary>
    /// Render a <see cref="ChatResponseFormat"/> into the OpenAI-compatible
    /// <c>response_format</c> object, or <c>null</c> when there is nothing to send.
    ///
    /// <para>A schema yields
    /// <c>{"type":"json_schema","json_schema":{name,schema,strict}}</c> — the shape the Rust
    /// reference sends. <c>strict</c> is always <c>true</c>: MEAI models no strict flag, and
    /// Rust's <c>ResponseFormat::json_schema</c> constructor is strict, so that is the
    /// behavior being matched. An unnamed schema falls back to <c>structured_output</c>, the
    /// same default Rust uses when a name sanitizes to empty.</para>
    ///
    /// <para>A schemaless <see cref="ChatResponseFormat.Json"/> yields plain
    /// <c>{"type":"json_object"}</c>. Rust has no such variant, but the platform type makes
    /// it reachable and silently dropping a caller's explicit request for JSON mode would be
    /// worse than sending the field OpenAI defines for it.</para>
    /// </summary>
    private static JsonObject? BuildResponseFormat(ChatResponseFormat? format)
    {
        if (format is not ChatResponseFormatJson json)
        {
            return null;
        }
        if (json.Schema is not { } schema)
        {
            return new JsonObject { ["type"] = "json_object" };
        }
        return new JsonObject
        {
            ["type"] = "json_schema",
            ["json_schema"] = new JsonObject
            {
                ["name"] = string.IsNullOrEmpty(json.SchemaName) ? "structured_output" : json.SchemaName,
                ["schema"] = JsonSerializer.SerializeToNode(schema, JsonOptions),
                ["strict"] = true,
            },
        };
    }

    private static JsonArray BuildMessages(IEnumerable<ChatMessage> messages)
    {
        var wire = new JsonArray();
        foreach (var message in messages)
        {
            // Tool results are their own `role: "tool"` messages, one per result, paired to the
            // assistant turn that requested them by tool_call_id.
            foreach (var result in message.Contents.OfType<FunctionResultContent>())
            {
                wire.Add(new JsonObject
                {
                    ["role"] = "tool",
                    ["tool_call_id"] = result.CallId,
                    ["content"] = result.Result?.ToString() ?? string.Empty,
                });
            }

            var calls = message.Contents.OfType<FunctionCallContent>().ToList();
            var text = string.Concat(message.Contents.OfType<TextContent>().Select(c => c.Text));
            // Images count as content: a turn may carry images ALONE, and dropping it here
            // would silently discard the whole message.
            var images = message.Contents.OfType<ImageUrlContent>().ToList();
            if (calls.Count == 0 && images.Count == 0 && string.IsNullOrEmpty(text))
            {
                continue;
            }

            var entry = new JsonObject { ["role"] = RoleName(message.Role), ["content"] = Multimodal.Content(text, images) };
            if (calls.Count > 0)
            {
                var toolCalls = new JsonArray();
                foreach (var call in calls)
                {
                    toolCalls.Add(new JsonObject
                    {
                        ["id"] = call.CallId,
                        ["type"] = "function",
                        ["function"] = new JsonObject
                        {
                            ["name"] = call.Name,
                            ["arguments"] = JsonSerializer.Serialize(call.Arguments ?? new Dictionary<string, object?>(), JsonOptions),
                        },
                    });
                }
                entry["tool_calls"] = toolCalls;
            }
            wire.Add(entry);
        }
        return wire;
    }

    private static JsonArray BuildTools(ChatOptions? options)
    {
        var wire = new JsonArray();
        foreach (var function in options?.Tools?.OfType<AIFunction>() ?? [])
        {
            wire.Add(new JsonObject
            {
                ["type"] = "function",
                ["function"] = new JsonObject
                {
                    ["name"] = function.Name,
                    ["description"] = function.Description,
                    ["parameters"] = JsonNode.Parse(function.JsonSchema.GetRawText()),
                },
            });
        }
        return wire;
    }

    private static string RoleName(ChatRole role) =>
        role == ChatRole.System ? "system" : role == ChatRole.User ? "user" : "assistant";

    // ── response parsing ────────────────────────────────────────────────────────────────────

    /// <summary>
    /// The model's arguments arrive as a raw JSON STRING; <see cref="FunctionCallContent"/> wants a
    /// dictionary. Unparseable or absent arguments become an empty dictionary rather than throwing
    /// — a malformed call should reach the tool and fail there with a message the model can read,
    /// not blow up the turn.
    /// </summary>
    private static Dictionary<string, object?> ParseArguments(string? raw)
    {
        if (string.IsNullOrWhiteSpace(raw))
        {
            return new Dictionary<string, object?>();
        }
        try
        {
            return JsonSerializer.Deserialize<Dictionary<string, object?>>(raw, JsonOptions) ?? new Dictionary<string, object?>();
        }
        catch (JsonException)
        {
            return new Dictionary<string, object?>();
        }
    }

    private static UsageDetails? ReadUsage(JsonObject? usage)
    {
        if (usage is null)
        {
            return null;
        }
        var prompt = usage["prompt_tokens"]?.GetValue<long>() ?? 0;
        var completion = usage["completion_tokens"]?.GetValue<long>() ?? 0;
        return new UsageDetails
        {
            InputTokenCount = prompt,
            OutputTokenCount = completion,
            TotalTokenCount = prompt + completion,
        };
    }

    private static async Task ThrowIfNotSuccessAsync(HttpResponseMessage response, CancellationToken cancellationToken)
    {
        if (response.IsSuccessStatusCode)
        {
            return;
        }
        var body = await response.Content.ReadAsStringAsync(cancellationToken).ConfigureAwait(false);
        throw new HttpRequestException($"Gateway {(int)response.StatusCode}: {body.Trim()}");
    }
}
