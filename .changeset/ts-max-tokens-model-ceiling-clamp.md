---
"@smooai/smooth-operator-core": patch
---

feat(ts): clamp `max_tokens` to the model's output ceiling (TypeScript parity)

The TypeScript core now mirrors the Rust engine's model-output ceiling clamp
(EPIC th-1cc9fa). `AgentOptions` gains `modelMaxOutput?: number` and a new exported
`effectiveMaxTokens(configured, ceiling?)` helper computes `min(maxTokens, ceiling)`
(floored at 1, `undefined`/`0` ⇒ graceful passthrough). Every model call — both the
non-streaming `run` and streaming `runStream` request builds — now sends the clamped
value, so a budget/policy `maxTokens` (which may be tuned high) can never exceed what
the model can physically emit. Without the clamp a reasoning model burns its budget
on `reasoning_content` and returns empty `content`, or the upstream 400s (e.g.
`groq-compound` caps output at 8192). The ceiling is sourced from the gateway's
`/model/info` by the consumer (kept out of the engine so it takes no LiteLLM-specific
HTTP). No behaviour change when `modelMaxOutput` is unset.
