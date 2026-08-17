using System.Text.Json.Nodes;
using Microsoft.Extensions.AI;

namespace SmooAI.SmoothOperator.Core;

/// <summary>An image attachment on a user message (multimodal turns). <c>Url</c> is a
/// <c>data:</c> URL (<c>data:image/png;base64,...</c>) or a remote <c>https</c> URL;
/// <c>Detail</c> ("low"/"high"/"auto") is an optional OpenAI vision hint, omitted from
/// the wire when null. Mirrors Rust's <c>conversation::ImageContent</c>. Pearl th-25ce5c.</summary>
public sealed record ImageContent(string Url, string? Detail = null);

/// <summary>Carries an <see cref="ImageContent"/> through MEAI's content model, which is the
/// only channel between the agent (where a host sets the images) and
/// <c>GatewayChatClient</c> (where the wire body is assembled).
/// <para>
/// MEAI's own <c>DataContent</c>/<c>UriContent</c> would nearly fit, but neither can express
/// the OpenAI <c>detail</c> hint the other four engines support — so this keeps parity rather
/// than silently dropping it.
/// </para></summary>
public sealed class ImageUrlContent : AIContent
{
    public ImageUrlContent(ImageContent image)
    {
        Image = image ?? throw new ArgumentNullException(nameof(image));
    }

    public ImageContent Image { get; }
}

/// <summary>Multimodal wire assembly. All the logic lives here; <c>GatewayChatClient</c> and
/// <c>SmoothAgent</c> each call it once, so this lands alongside the other workstreams on the
/// same body-assembly line.</summary>
public static class Multimodal
{
    /// <summary>Build the turn's user message, attaching <paramref name="images"/> when the host
    /// supplied any. No images ⇒ the plain text message, unchanged.</summary>
    public static ChatMessage UserMessage(string text, IReadOnlyList<ImageContent>? images)
    {
        if (images is not { Count: > 0 })
        {
            return new ChatMessage(ChatRole.User, text);
        }

        var contents = new List<AIContent>(images.Count + 1);
        if (!string.IsNullOrEmpty(text))
        {
            contents.Add(new TextContent(text));
        }
        foreach (var image in images)
        {
            contents.Add(new ImageUrlContent(image));
        }
        return new ChatMessage(ChatRole.User, contents);
    }

    /// <summary>A message's wire <c>content</c>: an OpenAI content-parts array when it carries
    /// images (text part first — omitted when the text is empty, since images may be sent alone —
    /// then one <c>image_url</c> part per image, in order), otherwise the plain string every turn
    /// has always sent. No images ⇒ byte-identical to before this existed.
    /// <para>
    /// The <c>type</c> discriminator on every part is load-bearing beyond this method:
    /// <c>CacheControl</c> decides whether it may wrap a message's content by scanning parts for a
    /// <c>type</c> that isn't "text". Drop it and that guard fails open — cache_control flattens
    /// the parts into a text block and the images vanish silently.
    /// </para></summary>
    public static JsonNode Content(string text, IReadOnlyList<ImageUrlContent> images)
    {
        ArgumentNullException.ThrowIfNull(images);
        if (images.Count == 0)
        {
            return JsonValue.Create(text)!;
        }

        var parts = new JsonArray();
        if (!string.IsNullOrEmpty(text))
        {
            parts.Add(new JsonObject { ["type"] = "text", ["text"] = text });
        }
        foreach (var content in images)
        {
            var imageUrl = new JsonObject { ["url"] = content.Image.Url };
            if (content.Image.Detail is not null)
            {
                imageUrl["detail"] = content.Image.Detail;
            }
            parts.Add(new JsonObject { ["type"] = "image_url", ["image_url"] = imageUrl });
        }
        return parts;
    }
}
