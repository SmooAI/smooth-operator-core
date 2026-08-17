---
"@smooai/smooth-operator-core": patch
---

feat(go): multimodal image attachments on user messages

Ports the Rust reference's multimodal turns (pearl th-25ce5c) to the Go engine.
A host that receives a chat turn carrying images sets `AgentOptions.NextUserImages`;
the agent attaches them to that one turn's user message, and the client emits them
as OpenAI `image_url` content parts — the standard shape every model we route
vision to (gemini-flash, gpt-4o, mimo-vl) speaks.

- `ImageContent{URL, Detail}` — a `data:` URL (`data:image/png;base64,...`) or a
  remote `https` URL, plus the optional OpenAI vision hint, omitted when empty.
- `ChatMessage.Images` — attachments on a USER message.
- `AgentOptions.NextUserImages` — the Go sibling of `AgentConfig::with_user_images`.

Wire shape matches Rust exactly: text part first (omitted when the text is empty,
since images may be sent alone), then one `image_url` part per image, in order.

**A turn without images is byte-identical to before.** `content` stays a plain
string unless a user message actually carries images — the negative case is what
the tests lead with, since every existing text-only turn depends on it.

Also pays off the `ponytail:` note prompt caching left behind: `wrapWithCacheControl`
now passes a content-parts array through untouched. Flattening it into a text block
would have silently dropped the images, and caching only applies to text prefixes
anyway (same reasoning as Rust's `wrap_with_cache_control`).

TypeScript, Python and .NET are deliberately NOT included — their request layers
are being rewritten by the http-client-parity work, and porting into a moving
target would just create conflicts. They follow once that lands.
