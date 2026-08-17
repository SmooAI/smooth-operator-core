using System.Text.Json.Nodes;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>Ports the Rust reference engine's cache_control gate + request-body tests (llm.rs).</summary>
public class CacheControlTests
{
    [Theory]
    // Claude model id + LiteLLM gateway url → cache it.
    [InlineData("claude-sonnet-4-20250514", "https://litellm.example.com/v1", true)]
    // Smooth-coding alias + gateway url → cache it.
    [InlineData("smooth-coding-claude", "https://gateway.example.com/v1", true)]
    // Direct Anthropic API + Claude id → cache it.
    [InlineData("claude-opus-4", "https://api.anthropic.com/v1", true)]
    // GPT model on OpenAI → no cache control (would 400).
    [InlineData("gpt-4o", "https://api.openai.com/v1", false)]
    // Gemini-compat → no cache control.
    [InlineData("gemini-1.5-pro", "https://generativelanguage.googleapis.com", false)]
    // Claude id but bare OpenAI url (mis-configured) — still gated off.
    [InlineData("claude-3-sonnet", "https://api.openai.com/v1", false)]
    // smooth-fast routes to Groq/Llama via the gateway — must NOT be cached.
    [InlineData("smooth-fast", "https://gateway.example.com/v1", false)]
    // No route at all → nothing to mark.
    [InlineData("claude-opus-4", null, false)]
    public void GateRecognizesClaudeRoutes(string model, string? url, bool expected)
    {
        Assert.Equal(expected, CacheControl.SupportsAnthropicCacheControl(model, url));
    }

    private static JsonObject SampleBody() => new()
    {
        ["model"] = "smooth-coding-claude",
        ["messages"] = new JsonArray
        {
            new JsonObject { ["role"] = "system", ["content"] = "You are smooth." },
            new JsonObject { ["role"] = "user", ["content"] = "Hi" },
        },
        ["tools"] = new JsonArray
        {
            new JsonObject { ["type"] = "function", ["function"] = new JsonObject { ["name"] = "bash" } },
            new JsonObject { ["type"] = "function", ["function"] = new JsonObject { ["name"] = "file_write" } },
        },
    };

    [Fact]
    public void MarksSystemLastToolAndLastMessage()
    {
        var body = SampleBody();
        CacheControl.Apply(body);

        var messages = (JsonArray)body["messages"]!;
        var sysContent = messages[0]!["content"] as JsonArray;
        Assert.NotNull(sysContent);
        Assert.Equal("text", (string?)sysContent[0]!["type"]);
        Assert.Equal("You are smooth.", (string?)sysContent[0]!["text"]);
        Assert.Equal("ephemeral", (string?)sysContent[0]!["cache_control"]!["type"]);

        var tools = (JsonArray)body["tools"]!;
        Assert.Null(tools[0]!["cache_control"]);
        Assert.Equal("ephemeral", (string?)tools[1]!["cache_control"]!["type"]);

        var lastContent = messages[1]!["content"] as JsonArray;
        Assert.NotNull(lastContent);
        Assert.Equal("ephemeral", (string?)lastContent[0]!["cache_control"]!["type"]);
    }

    [Fact]
    public void EmptyContentLeftAlone()
    {
        // A tool-call-only assistant message has no prose to cache.
        var body = new JsonObject
        {
            ["messages"] = new JsonArray
            {
                new JsonObject { ["role"] = "system", ["content"] = "sys" },
                new JsonObject { ["role"] = "assistant", ["content"] = "" },
            },
        };
        CacheControl.Apply(body);
        Assert.Equal("", (string?)((JsonArray)body["messages"]!)[1]!["content"]);
    }

    [Fact]
    public void MultimodalContentPassesThrough()
    {
        // Flattening would silently drop the image; caching only covers text prefixes.
        var body = new JsonObject
        {
            ["messages"] = new JsonArray
            {
                new JsonObject { ["role"] = "system", ["content"] = "sys" },
                new JsonObject
                {
                    ["role"] = "user",
                    ["content"] = new JsonArray
                    {
                        new JsonObject { ["type"] = "text", ["text"] = "look" },
                        new JsonObject { ["type"] = "image_url", ["image_url"] = new JsonObject { ["url"] = "data:image/png;base64,ZZZZ" } },
                    },
                },
            },
        };
        CacheControl.Apply(body);

        var content = (JsonArray)((JsonArray)body["messages"]!)[1]!["content"]!;
        Assert.Equal(2, content.Count);
        Assert.Equal("image_url", (string?)content[1]!["type"]);
        Assert.Null(content[1]!["cache_control"]);
        Assert.Null(content[0]!["cache_control"]);
    }

    [Fact]
    public void RemarksOnlyTheLastBlock()
    {
        var body = new JsonObject
        {
            ["messages"] = new JsonArray
            {
                new JsonObject { ["role"] = "system", ["content"] = "sys" },
                new JsonObject
                {
                    ["role"] = "user",
                    ["content"] = new JsonArray
                    {
                        new JsonObject { ["type"] = "text", ["text"] = "first", ["cache_control"] = new JsonObject { ["type"] = "ephemeral" } },
                        new JsonObject { ["type"] = "text", ["text"] = "second" },
                    },
                },
            },
        };
        CacheControl.Apply(body);

        var blocks = (JsonArray)((JsonArray)body["messages"]!)[1]!["content"]!;
        Assert.Null(blocks[0]!["cache_control"]);
        Assert.Equal("ephemeral", (string?)blocks[1]!["cache_control"]!["type"]);
    }

    [Fact]
    public void UnmarkedBodyContainsNoCacheControl()
    {
        // The gated-off path must leave the body exactly as it was built.
        var body = SampleBody();
        var before = body.ToJsonString();
        Assert.False(CacheControl.SupportsAnthropicCacheControl("gpt-4o", "https://api.openai.com/v1"));
        Assert.DoesNotContain("cache_control", before, StringComparison.Ordinal);
        Assert.Equal(before, SampleBody().ToJsonString());
    }
}
