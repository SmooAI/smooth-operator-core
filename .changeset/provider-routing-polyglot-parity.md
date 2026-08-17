---
'@smooai/smooth-operator-core': patch
---

Port provider routing to the Go, TypeScript, Python and .NET engines.

Routing was Rust-only, so the other four engines could talk to a gateway but had no say in *which* model a given call should use — every activity got whatever single client the consumer wired up. Each engine now ships the same three pieces:

- **`ProviderRegistry`** — provider credentials/URLs plus a per-activity routing table. Six semantic slots (`Coding`, `Reasoning`, `Reviewing`, `Judge`, `Summarize`, `Fast`) each resolve to a `ModelSlot`, and resolution walks the slot's fallback chain until it finds a registered provider. An unregistered provider with no fallback is an error, never a silent substitution to somewhere else. Five presets (Smoo AI gateway, OpenRouter/LLM Gateway low-cost, OpenAI, Anthropic) and nine provider factories are included; the hosted gateway is opt-in and never the default.
- **Per-model wire quirks** — case-insensitive substring lookup on the concrete upstream name, so minor version drift still hits its entry.
- **LiteLLM alias resolution** — `GET /model/info` recovers the gateway's `alias → upstream` map, alias-sorted so diagnostics print the same order every run.

The registry reads and writes the same `~/.smooth/providers.json` the Rust CLI does: snake_case keys, optional slots omitted rather than written as null, and the legacy `thinking` / `planning` field names still migrating onto the merged `reasoning` slot. Each engine also gains a `clientFor(activity)` bridge that turns a resolved route into that language's gateway client, refusing an Anthropic-dialect provider rather than speaking OpenAI's wire format at it.

Routing is the expensive place to diverge — a slot resolving to the wrong model or base URL sends real traffic and real money somewhere nobody intended, and it looks like it is working. So the values are pinned by a shared corpus at `spec/providers/routing.json`, generated from the Rust reference: all five engines (Rust included) replay it and assert every preset slot resolves to the same model, URL, key and wire format, that quirks match, and that URL-building and alias parsing agree.
