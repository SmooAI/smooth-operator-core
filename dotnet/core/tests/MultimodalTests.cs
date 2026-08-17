using System.Text.Json;
using System.Text.Json.Nodes;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Multimodal image attachments (pearl th-25ce5c), ported from the Rust reference. The
/// load-bearing property is the NEGATIVE one: a turn without images must be byte-identical
/// to before the field existed.
/// </summary>
public class MultimodalTests
{
    private static List<ImageUrlContent> Images(params ImageContent[] images) =>
        images.Select(i => new ImageUrlContent(i)).ToList();

    [Fact]
    public void Content_NoImages_StaysAPlainString()
    {
        var content = Multimodal.Content("hello", new List<ImageUrlContent>());
        Assert.Equal(JsonValueKind.String, content.GetValueKind());
        Assert.Equal("hello", content.GetValue<string>());
    }

    [Fact]
    public void Content_EmitsTextPartThenOneImagePartPerImage()
    {
        var content = Multimodal.Content(
            "what is this?",
            Images(new ImageContent("data:image/png;base64,AAAA"), new ImageContent("https://x/y.jpg", "high")));

        Assert.Equal(
            """[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},{"type":"image_url","image_url":{"url":"https://x/y.jpg","detail":"high"}}]""",
            content.ToJsonString());
    }

    [Fact]
    public void Content_ImagesAlone_OmitsTheTextPart()
    {
        var content = Multimodal.Content(string.Empty, Images(new ImageContent("data:image/png;base64,ZZZZ")));
        var parts = Assert.IsType<JsonArray>(content);
        Assert.Single(parts);
        Assert.Equal("image_url", (string?)parts[0]!["type"]);
    }

    [Fact]
    public void UserMessage_NoImages_IsAnOrdinaryTextTurn()
    {
        var message = Multimodal.UserMessage("hello", null);
        Assert.Equal(ChatRole.User, message.Role);
        Assert.Empty(message.Contents.OfType<ImageUrlContent>());
        Assert.Equal("hello", message.Text);
    }

    [Fact]
    public void UserMessage_CarriesImagesThroughMeaiContents()
    {
        var message = Multimodal.UserMessage("look", new[] { new ImageContent("data:image/png;base64,AAAA") });
        Assert.Single(message.Contents.OfType<ImageUrlContent>());
        Assert.Equal("look", message.Text);
    }

    [Fact]
    public void VisionTurn_ThroughAClaudeRoute_StillCarriesTheImage()
    {
        // cache_control marks the LAST message, which in a vision turn IS the image-bearing
        // one. Flattening it into a text block drops the images silently.
        var body = new JsonObject
        {
            ["model"] = "claude-sonnet-4-5",
            ["messages"] = new JsonArray(
                new JsonObject { ["role"] = "system", ["content"] = "be helpful" },
                new JsonObject
                {
                    ["role"] = "user",
                    ["content"] = Multimodal.Content("what is this?", Images(new ImageContent("data:image/png;base64,AAAA"))),
                }),
        };

        CacheControl.Apply(body);

        var content = Assert.IsType<JsonArray>(body["messages"]![1]!["content"]);
        Assert.Contains(content, p => (string?)p!["type"] == "image_url");
        // Passed through untouched — no marker smuggled onto an image part.
        Assert.All(content, p => Assert.Null(p!["cache_control"]));
    }

    [Fact]
    public void TextOnlyTurn_StillCachesOnTheSameRoute()
    {
        var body = new JsonObject
        {
            ["model"] = "claude-sonnet-4-5",
            ["messages"] = new JsonArray(
                new JsonObject { ["role"] = "system", ["content"] = "be helpful" },
                new JsonObject { ["role"] = "user", ["content"] = Multimodal.Content("no images", new List<ImageUrlContent>()) }),
        };

        CacheControl.Apply(body);

        // Sanity: the guard is scoped to multimodal content, it didn't disable caching.
        Assert.Contains("cache_control", body.ToJsonString(), StringComparison.Ordinal);
    }
}
