using System.Text.Json.Nodes;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// Anthropic prompt-cache markers on the outbound request — the C# port of the Rust
/// reference's <c>supports_anthropic_cache_control</c> + <c>apply_cache_control</c>
/// (llm.rs), and the wire half of <see cref="PromptCache"/>.
///
/// <para>Kept standalone rather than inline in <see cref="GatewayChatClient"/> so the
/// request builder needs a single gated call — the marking rules live here.</para>
/// </summary>
public static class CacheControl
{
    /// <summary>Anthropic's default 5-minute TTL marker.</summary>
    private static JsonObject Ephemeral() => new() { ["type"] = "ephemeral" };

    /// <summary>
    /// Does the configured upstream understand Anthropic-shaped <c>cache_control</c>?
    ///
    /// <para>True when the model id looks Claude-ish, or is one of the known semantic
    /// gateway aliases that route to Claude, AND the api base looks like a LiteLLM-style
    /// gateway or <c>anthropic.*</c> directly.</para>
    ///
    /// <para>We deliberately do NOT send these to bare OpenAI / Gemini / Groq endpoints —
    /// they 400 on unknown extension fields. A LiteLLM gateway's
    /// <c>cache_control_injection_points</c> config is what actually forwards the markers
    /// to Anthropic; without that gateway-side change this is a no-op.</para>
    /// </summary>
    public static bool SupportsAnthropicCacheControl(string? model, string? apiBaseUrl)
    {
        if (string.IsNullOrEmpty(model) || string.IsNullOrEmpty(apiBaseUrl)) return false;
        var m = model.ToLowerInvariant();
        var u = apiBaseUrl.ToLowerInvariant();
        var looksClaude = m.Contains("claude", StringComparison.Ordinal)
            || m.Contains("sonnet", StringComparison.Ordinal)
            || m.Contains("opus", StringComparison.Ordinal)
            || m.Contains("haiku", StringComparison.Ordinal);
        // The generic `smooth-` prefix alone isn't enough — `smooth-fast` routes to a
        // Groq/Llama model, which would 400 on cache_control.
        var isClaudeAlias = m.StartsWith("smooth-coding", StringComparison.Ordinal)
            || m.StartsWith("smooth-thinking", StringComparison.Ordinal)
            || m.StartsWith("smooth-planning", StringComparison.Ordinal)
            || m.StartsWith("smooth-reviewing", StringComparison.Ordinal);
        var urlIsGateway = u.Contains("litellm", StringComparison.Ordinal) || u.Contains("gateway", StringComparison.Ordinal);
        var urlIsAnthropic = u.Contains("anthropic.", StringComparison.Ordinal);
        return (looksClaude || isClaudeAlias) && (urlIsGateway || urlIsAnthropic);
    }

    /// <summary>
    /// Attach <c>cache_control: ephemeral</c> to the strategic prefix boundaries, in place:
    /// <list type="number">
    /// <item>The last system message — caches the system prompt.</item>
    /// <item>The last tool definition — caches the tool block + the system prefix ahead of
    /// it. Highest-ROI breakpoint: the tool registry is large and near-constant in a run.</item>
    /// <item>The last message in history — caches the running conversation, so each turn
    /// inside the 5-minute window pays only for the new delta.</item>
    /// </list>
    /// Marking a block caches THAT block plus everything before it, so only the last block
    /// of each prefix we want to reuse needs a marker.
    /// </summary>
    public static void Apply(JsonObject body)
    {
        ArgumentNullException.ThrowIfNull(body);
        var messages = body["messages"] as JsonArray;
        var tools = body["tools"] as JsonArray;

        // 1. Last system message.
        if (messages is not null)
        {
            for (var i = messages.Count - 1; i >= 0; i--)
            {
                if (messages[i] is JsonObject msg && (string?)msg["role"] == "system")
                {
                    msg["content"] = WrapWithCacheControl(msg["content"]);
                    break;
                }
            }
        }

        // 2. Last tool — covers the whole tools array plus the system prefix.
        if (tools is { Count: > 0 } && tools[^1] is JsonObject lastTool)
        {
            lastTool["cache_control"] = Ephemeral();
        }

        // 3. Last message, so turn-by-turn history caching extends. Skipped when the only
        //    message is the system we just marked (avoid double-marking it).
        if (messages is { Count: > 1 } && messages[^1] is JsonObject last)
        {
            last["content"] = WrapWithCacheControl(last["content"]);
        }
    }

    /// <summary>
    /// Rewrite string content into the single-text-block form carrying the marker.
    ///
    /// <para>Empty/absent content (a tool-call-only assistant message) is returned
    /// untouched: there is nothing to cache on it, and the marker on the last block before
    /// the assistant turn already covers the prefix. Content already in array form — either
    /// re-marked blocks or OpenAI multimodal parts — is handled without flattening: for
    /// blocks the marker moves to the last one, and anything carrying a non-text part (an
    /// image) passes through unchanged, since flattening would silently drop the image and
    /// prompt caching only applies to text prefixes anyway.</para>
    /// </summary>
    private static JsonNode? WrapWithCacheControl(JsonNode? content)
    {
        if (content is JsonArray parts)
        {
            if (parts.Count == 0) return content;
            // Multimodal: leave images (and their sibling text parts) exactly as they are.
            foreach (var part in parts)
            {
                if (part is JsonObject po && po["type"] is not null && (string?)po["type"] != "text") return content;
            }
            var blocks = new JsonArray();
            for (var i = 0; i < parts.Count; i++)
            {
                var rebuilt = new JsonObject();
                if (parts[i] is JsonObject po)
                {
                    foreach (var kv in po)
                    {
                        if (kv.Key != "cache_control") rebuilt[kv.Key] = kv.Value?.DeepClone();
                    }
                }
                if (i == parts.Count - 1) rebuilt["cache_control"] = Ephemeral();
                blocks.Add(rebuilt);
            }
            return blocks;
        }

        var text = (string?)content;
        if (string.IsNullOrEmpty(text)) return content;
        return new JsonArray
        {
            new JsonObject { ["type"] = "text", ["text"] = text, ["cache_control"] = Ephemeral() },
        };
    }
}
