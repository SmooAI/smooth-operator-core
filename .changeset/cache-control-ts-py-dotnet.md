---
'@smooai/smooth-operator-core': patch
---

feat(ts,python,dotnet): send Anthropic `cache_control` markers on the request

Completes prompt-caching parity. The previous change ported the `PromptCache`
static/dynamic split to all five engines but landed the wire half — the
`cache_control` markers that make Anthropic actually cache the prefix — in Rust
and Go only, because TypeScript, Python and .NET had no request builder of their
own to attach them to. Their gateway clients have since shipped, so all five now
send the markers.

Marker placement matches Rust exactly: `ephemeral` on the last system message
(rewritten into Anthropic block form), on the **last** tool in the tools array
(the highest-ROI breakpoint — the tool registry is large and near-constant within
a run), and on the last history message so turn-by-turn caching extends. Marking
a block caches that block plus everything before it, so only the last block of
each reused prefix carries one.

The gate is the same everywhere: a Claude-ish model id _or_ a known Claude-routing
`smooth-*` alias, AND a LiteLLM-style gateway or `anthropic.*` base URL. Bare
OpenAI/Gemini/Groq endpoints 400 on unknown extension fields, and `smooth-fast`
routes to Groq, so both stay off.

**Where the base URL comes from.** The gate needs the route, which the engine
previously had no way to see — the agent is handed a client, not a URL. Each
client now reports its own: TypeScript's `ChatClientLike` gained an optional
`apiBaseUrl` that `gatewayClientFrom` fills from the OpenAI SDK, Python's
`GatewayLlmProvider` exposes `api_base_url`, and .NET reads its existing
`_endpoint`. A client that doesn't report one — every mock — never gets markers,
so existing tests and any hand-rolled client are unaffected.

**Multimodal content is passed through, not flattened.** Rust learned this the
hard way (pearl th-25ce5c): wrapping a message that carries image parts into a
text block silently drops the images, and the last message in a vision turn is
exactly the image-bearing one. All three ports detect a non-text part and leave
the content untouched; caching only applies to text prefixes anyway. Empty
content (a tool-call-only assistant message) is likewise left alone, and content
already in block form has its stale marker cleared before the last block is
re-marked.

The marking logic lives in a standalone module per language (`cacheControl.ts`,
`cache_control.py`, `CacheControl.cs`) so the agent/request path needed only a
single gated call — deliberate, to keep the diff in those heavily-shared files to
a couple of lines.

29 new tests (8 TypeScript, 8 Python, 13 .NET), including end-to-end assertions
that an agent driven by a client reporting no base URL sends a body with no
`cache_control` anywhere.
