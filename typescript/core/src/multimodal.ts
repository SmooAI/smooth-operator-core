/**
 * Multimodal image attachments on user messages (pearl th-25ce5c).
 *
 * All the logic lives here; the agent's body-assembly sites call {@link userContent}
 * once each, so this can land alongside the other workstreams on that same line.
 */

/**
 * An image attachment on a user message. `url` is a `data:` URL
 * (`data:image/png;base64,...`) or a remote `https` URL; `detail`
 * (`"low"`/`"high"`/`"auto"`) is an optional OpenAI vision hint, omitted when unset.
 */
export interface ImageContent {
    url: string;
    detail?: string;
}

/**
 * One part of an OpenAI multimodal `content` array.
 *
 * The `type` discriminator is REQUIRED on every part and is load-bearing beyond
 * this module: prompt caching (`cacheControl.ts`) decides whether it may wrap a
 * message's content by scanning parts for a `type` that isn't `"text"`. Drop it
 * and that guard fails open — cache_control flattens the parts into a text block
 * and the images vanish silently.
 */
export type ContentPart =
    | { type: 'text'; text: string }
    | { type: 'image_url'; image_url: { url: string; detail?: string } };

/**
 * Build a user message's wire `content`: an OpenAI content-parts array when the
 * turn carries images (text part first — omitted when the text is empty, since
 * images may be sent alone — then one `image_url` part per image, in order),
 * otherwise the plain string every turn has always sent.
 *
 * No images ⇒ the exact input string, so every text-only turn stays
 * byte-identical to before this existed.
 */
export function userContent(text: string, images?: readonly ImageContent[]): string | ContentPart[] {
    if (!images || images.length === 0) return text;

    const parts: ContentPart[] = [];
    if (text !== '') parts.push({ type: 'text', text });
    for (const image of images) {
        parts.push({
            type: 'image_url',
            image_url: image.detail === undefined ? { url: image.url } : { url: image.url, detail: image.detail },
        });
    }
    return parts;
}
