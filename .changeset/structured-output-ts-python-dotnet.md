---
"@smooai/smooth-operator-core": patch
---

feat(ts,python,dotnet): structured output — `response_format` + JSON parsing, completing the five-engine set

Finishes the port Go started (core#130). Structured output — a guaranteed-JSON
answer conforming to a caller-supplied JSON Schema — now exists in all five
engines, so `docs/Polyglot-Engines.md` stops listing it among the Rust-only
surfaces and gets a row of its own.

Each engine uses its own idiom rather than a transliterated Rust enum:

- **TypeScript** — `jsonSchemaFormat(name, schema)` + `responseFormatField(format)`,
  a spreadable fragment matching this package's existing `metadataField`, so an
  unset format contributes nothing: `JSON.stringify({model, ...responseFormatField()})`
  is `{"model":"m"}`. `structuredJson<T>(response)` parses it back.
- **Python** — `json_schema_format` + `response_format_field` returning a mapping
  to splat into `chat.completions.create`, mirroring the `**({"metadata": ...})`
  idiom already there. `structured_json(response)` parses it back.
- **.NET** — **no new type at all.** `Microsoft.Extensions.AI` already models this
  as `ChatOptions.ResponseFormat` / `ChatResponseFormat.ForJsonSchema`, so
  `GatewayChatClient` maps the platform's own `ChatResponseFormatJson` onto the
  wire object; `StructuredJson()` / `DeserializeJson<T>()` are extension methods on
  `ChatResponse`. Inventing a parallel `ResponseFormat` beside the platform's would
  have been a second way to say the same thing.

**Byte-identical when unset** in all three, each with a test that fails if the
field ever starts serializing as `null` or `{}`.

Two deliberate deviations, both because matching Rust's *signature* would have
meant not matching its *behavior*:

- **No `deserializeJson` in TypeScript or Python.** Rust's version validates
  against a concrete type through `serde`. TypeScript has no runtime types and
  Python has no equivalent without a validation dependency, so there it would be
  `structuredJson` plus a cast — an API implying a guarantee the engine cannot
  make. Go and .NET, which really can deserialize, have it.
- **.NET sends `{"type":"json_object"}` for a schemaless `ChatResponseFormat.Json`.**
  Rust has no such variant, but the platform type makes it reachable, and silently
  dropping a caller's explicit request for JSON mode is worse than sending the
  field OpenAI defines for it.

Rust's Anthropic-native path (no `response_format` there, so it forces a synthetic
tool whose `input_schema` IS the schema) has no analogue in the four ports, which
talk to OpenAI-compatible endpoints only. Stated in each port's source rather than
silently omitted.

28 new tests across the three: TypeScript 279 passed, Python 317 passed, .NET 285
passed.
