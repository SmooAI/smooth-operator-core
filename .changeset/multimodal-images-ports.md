---
"@smooai/smooth-operator-core": patch
---

feat(ts,python,dotnet): multimodal image attachments on user messages

Completes the multimodal port (pearl th-25ce5c) — Rust had it, Go landed in #147,
and these are the remaining three. A host that receives a chat turn carrying images
sets `nextUserImages` / `next_user_images` / `NextUserImages`; the agent attaches
them to that one turn's user message, and the body-assembly site emits OpenAI
`image_url` content parts.

Wire shape matches Rust exactly in all four: text part first (omitted when the text
is empty, since images may be sent alone), then one `image_url` part per image, in
order, with `detail` omitted when unset.

**A turn without images is byte-identical to before** — `content` stays a plain
string unless a user message actually carries images. Each language's tests lead
with that negative case.

Logic lives in a standalone module per language (`multimodal.ts`, `multimodal.py`,
`Multimodal.cs`) with a single call at the shared body-assembly line, following the
convention prompt caching established — so the three workstreams on that line rebase
past each other instead of colliding.

.NET differs in shape, because its assembly site is `GatewayChatClient.BuildMessages`
rather than the agent: images ride agent→client through MEAI's content model as an
`ImageUrlContent : AIContent`. MEAI's own `DataContent`/`UriContent` nearly fit but
cannot express the OpenAI `detail` hint the other engines support. `BuildMessages`
also had to stop treating an empty-text message as skippable — a turn may carry
images alone, and the old guard would have dropped it silently.

**Interaction with prompt caching, which is the real hazard here.** cache_control
marks the LAST message in history, which in a vision turn IS the image-bearing one;
flattening it into a text block would silently drop the images. The caching ports
already guard this by passing content through untouched when any part has a `type`
other than `"text"` — so these parts always emit `type`, and each language now has a
regression test driving a vision turn through a Claude-routing gateway and asserting
the image survives. Verified by mutation: removing the discriminator makes that test
fail, which is exactly the silent failure it exists to catch.
