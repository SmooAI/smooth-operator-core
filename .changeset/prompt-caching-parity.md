---
'@smooai/smooth-operator-core': patch
---

feat(go,ts,python,dotnet,rust): port prompt caching — the static/dynamic split everywhere, `cache_control` wire markers in Go

Prompt caching in the Rust reference is two separable pieces, and they had
different parity gaps:

**1. The static/dynamic system-prompt split (`PromptCache`) — now in all five.**
A system prompt has two halves with very different churn rates: role
instructions and tool schemas barely change, while project context
(AGENTS.md / CLAUDE.md) changes every turn. Anthropic's cache keys on a
_prefix_, so putting the volatile half first invalidates everything.
`__PROMPT_CACHE_BOUNDARY__` splits them — above it is static and hashed once for
cache-key dedup, below it is dynamic and swappable via `updateDynamic` without
busting the static prefix. `fullPrompt()` reassembles it for the agent's
instructions, and a prompt with no marker round-trips unchanged (all dynamic,
nothing falsely claimed cacheable).

This was `pub` inside Rust's `conversation` module but **never re-exported at the
crate root**, so no embedder could reach it. Rust now exports `PromptCache`
alongside `Conversation`.

**2. The `cache_control` request markers — Rust + Go.** `ephemeral` markers go on
the three strategic prefix boundaries: the last system message (rewritten into
Anthropic block form), the LAST tool in the tools array (the highest-ROI
breakpoint — the tool registry is large and near-constant within a run), and the
last history message (so turn-by-turn caching extends). Gated exactly like Rust:
a Claude-ish model id _or_ a known Claude-routing `smooth-*` alias, AND a
LiteLLM-style gateway or `anthropic.*` base URL. Bare OpenAI/Gemini/Groq
endpoints 400 on unknown extension fields, and `smooth-fast` routes to Groq, so
both stay off.

**TypeScript, Python and .NET do not get the wire half yet** — deliberately, not
by oversight. Those three take an injected chat client and have no request
builder of their own, so there is literally nowhere to attach a marker until the
native HTTP client lands. They get the full `PromptCache` split now and the
markers when that request layer merges.

**Byte-identical when off.** Go's `wireMessage.Content` widened from `string` to
`any` to carry either a plain string or a block array; a string in an `any` field
marshals identically, `cache_control` fields are `omitempty`, and the gate needs
an api base it is only given by the real client. A regression test asserts that
a request built with no URL is byte-for-byte the same as one to a gated-off
upstream, and that neither contains `cache_control`.

One deliberate divergence: `staticHash()` is FNV-1a in the ports rather than
Rust's `DefaultHasher`, which is not reproducible across languages or even
across Rust releases. The hash is process-local cache-key dedup and never goes
on the wire, so the ported contract is the behavior — same static text hashes
the same, different text differs, `updateDynamic` never changes it — not the
literal value.

Rust's tests ported: 7 `PromptCache` tests to each of the four ports, plus Rust's
`cache_control` gate and request-body tests to Go (37 new tests total).
