---
"@smooai/smooth-operator-core": patch
---

feat(dotnet): clamp `max_tokens` to the model's output ceiling (.NET parity)

`AgentOptions` gains `MaxOutputTokens` (the budget), `ModelMaxOutputTokens` (the
model's hard output ceiling), and `EffectiveMaxTokens` = `min(budget, ceiling)`
(never 0; `null` budget = leave `max_tokens` unset; `null`/≤0 ceiling = graceful
passthrough). `SmoothAgent` now sends the clamped value as the request's
`ChatOptions.MaxOutputTokens`. Mirrors the Rust engine's `with_model_ceiling` /
`effective_max_tokens` so a policy/budget `max_tokens` can never exceed what the
model can physically emit — otherwise a reasoning model burns its budget on
reasoning and returns empty, or the upstream 400s (e.g. `groq-compound`'s 8192
output cap under a 32768 budget). The ceiling is sourced from the gateway's
`/model/info` by the consumer (kept out of the engine). EPIC th-1cc9fa.
