using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// Structured output — reading back the guaranteed-JSON answer a
/// <see cref="ChatOptions.ResponseFormat"/> asked for. Port of the Rust reference's
/// <c>LlmResponse::structured_json</c> / <c>LlmResponse::deserialize_json</c>
/// (<c>rust/smooth-operator-core/src/llm.rs</c>, SMOODEV-1472).
///
/// <para>The REQUEST side needs nothing from this engine:
/// <c>Microsoft.Extensions.AI</c> already models the constraint as
/// <see cref="ChatResponseFormat.ForJsonSchema(JsonElement, string, string)"/>, and
/// <see cref="GatewayChatClient"/> maps it onto the OpenAI-compatible
/// <c>response_format</c> field. So unlike the Go/TypeScript/Python ports there is no
/// <c>ResponseFormat</c> type here — inventing one alongside the platform's would be a
/// second way to say the same thing.</para>
///
/// <code>
/// var options = new ChatOptions { ResponseFormat = ChatResponseFormat.ForJsonSchema(schema, "weather") };
/// var response = await client.GetResponseAsync(messages, options);
/// var report = response.DeserializeJson&lt;WeatherReport&gt;();
/// </code>
/// </summary>
public static class StructuredOutput
{
    /// <summary>First 200 characters — matches the Rust reference's truncation.</summary>
    private static string Snippet(string text) => text.Length <= 200 ? text : text[..200];

    /// <summary>
    /// Parse the response text as a JSON node. For a structured-output response this is the
    /// schema-conforming object the model produced.
    /// </summary>
    /// <exception cref="InvalidOperationException">
    /// The response text is empty or is not valid JSON. The message quotes a truncated snippet
    /// of the offending content, so a model that ignored the schema is diagnosable from the
    /// error alone. Never silently returns an empty value.
    /// </exception>
    public static JsonNode StructuredJson(this ChatResponse response)
    {
        ArgumentNullException.ThrowIfNull(response);
        var content = (response.Text ?? string.Empty).Trim();
        if (content.Length == 0)
        {
            throw new InvalidOperationException("structured output: model returned empty content (expected a JSON object)");
        }
        try
        {
            return JsonNode.Parse(content)
                ?? throw new InvalidOperationException($"structured output: response content was not valid JSON (null): {Snippet(content)}");
        }
        catch (JsonException error)
        {
            throw new InvalidOperationException(
                $"structured output: response content was not valid JSON ({error.Message}): {Snippet(content)}", error);
        }
    }

    /// <summary>
    /// Parse the response text into <typeparamref name="T"/> — the analogue of Rust's
    /// <c>deserialize_json</c>.
    /// </summary>
    /// <exception cref="InvalidOperationException">
    /// The response text is empty, is not valid JSON, or does not match the shape of
    /// <typeparamref name="T"/>.
    /// </exception>
    public static T DeserializeJson<T>(this ChatResponse response, JsonSerializerOptions? options = null)
    {
        ArgumentNullException.ThrowIfNull(response);
        var content = (response.Text ?? string.Empty).Trim();
        if (content.Length == 0)
        {
            throw new InvalidOperationException(
                "structured output: model returned empty content (expected JSON for the requested type)");
        }
        try
        {
            return JsonSerializer.Deserialize<T>(content, options ?? new JsonSerializerOptions(JsonSerializerDefaults.Web))
                ?? throw new InvalidOperationException(
                    $"structured output: could not deserialize response into the requested type (null): {Snippet(content)}");
        }
        catch (JsonException error)
        {
            throw new InvalidOperationException(
                $"structured output: could not deserialize response into the requested type ({error.Message}): {Snippet(content)}", error);
        }
    }
}
