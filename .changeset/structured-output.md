---
'@smooai/smooth-operator-core-monorepo': minor
---

Add structured output (schema-constrained JSON responses) to the LLM client (SMOODEV-1472).

New public API:

- `ResponseFormat` enum (`JsonSchema { name, schema, strict }`) with a `ResponseFormat::json_schema(name, schema)` constructor (defaults `strict = true`).
- `LlmClient::chat_structured(messages, &ResponseFormat)` and the lower-level `LlmClient::chat_with_format(messages, tools, Option<&ResponseFormat>)`. `chat` now delegates to `chat_with_format(.., None)`.
- `LlmResponse::structured_json() -> serde_json::Value` and `LlmResponse::deserialize_json::<T>()` — both surface a clear error (never a silent empty value) when the model returned empty or non-JSON content.
- `LlmProvider::chat_structured` trait method; `MockLlmClient` records the requested `ResponseFormat` on `RecordedCall.response_format` for assertions.

Provider handling:

- **OpenAI-compatible** (LiteLLM gateway, etc.): serialized on `/chat/completions` as `response_format: { type: "json_schema", json_schema: { name, schema, strict } }`.
- **Anthropic-native** (`/v1/messages`): achieved via a forced single tool call — a synthetic tool whose `input_schema` is the requested schema, forced with `tool_choice: { type: "tool", name }`; the tool's `input` is surfaced back as the JSON content string.

Streaming structured output and agent-level (`Agent::run`) wiring are deliberately deferred as follow-ups.
