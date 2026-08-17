---
"@smooai/smooth-operator-core": patch
---

feat(go): structured output — `response_format` on the request, JSON helpers on the response

First port of the Rust reference's structured output (SMOODEV-1472) — a
guaranteed-JSON answer conforming to a caller-supplied JSON Schema. None of the
four ports had it; Go goes first because its `GatewayClient` owns its own HTTP
request builder and is stable on main. TypeScript, Python and .NET follow once
the in-flight HTTP/request-layer work lands there, so this adds a field to a
settled builder instead of racing one.

- **`ResponseFormat`** (`go/core/structured.go`) + `JSONSchemaFormat(name, schema)`,
  the analogue of Rust's `ResponseFormat::json_schema` (strict by default). Rust
  models this as a single-variant enum; Go uses a struct until a second variant
  exists.
- **`ChatRequest.ResponseFormat`** serializes as
  `response_format: {"type":"json_schema","json_schema":{name,schema,strict}}`.
  Pointer + `omitempty`, so an unset format leaves the request **byte-identical**
  to before — that is the parity bar, and there is a test that fails if the field
  ever starts emitting `null`.
- **`ChatResponse.StructuredJSON()` / `.DeserializeJSON(&target)`** mirror Rust's
  `structured_json` / `deserialize_json`, including the error text and the
  200-character (not byte) content snippet, so a model that ignores the schema is
  diagnosable from the error alone rather than surfacing as an empty value.
- The mock records the requested format for free — Go's `MockLlmProvider`
  captures the whole `ChatRequest` — which is what Rust's `RecordedCall.response_format`
  exists to provide.

Go's client speaks only the OpenAI-compatible endpoint, so Rust's Anthropic-native
path (no `response_format` field there; it forces a synthetic single tool call
whose `input_schema` IS the schema) has nothing to mirror here and is called out
in the source rather than silently omitted.

11 new tests, mirroring the Rust reference's own wire assertions
(`openai_request_carries_response_format_json_schema` and
`no_response_format_is_omitted_from_the_wire`) one-for-one.
