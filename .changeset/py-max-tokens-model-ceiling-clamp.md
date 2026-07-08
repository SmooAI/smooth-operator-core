---
"@smooai/smooth-operator-core": patch
---

feat(llm,python): clamp `max_tokens` to the model's output ceiling (Python parity)

Python parity for the Rust `LlmClient` output-ceiling clamp. `AgentOptions` gains a
`model_max_output: int | None` field and the engine now sends
`effective_max_tokens(max_tokens, model_max_output)` =
`min(max_tokens, ceiling)` on both the streaming and non-streaming chat paths (`None`
/ non-positive ceiling ⇒ graceful passthrough, no behaviour change). A new
`effective_max_tokens` helper is exported for consumers. This stops a policy/budget
`max_tokens` from exceeding what a model can physically emit — which otherwise makes
a reasoning model burn its budget and return empty, or 400s upstream (e.g.
`groq-compound`'s 8192 output cap). The ceiling is sourced from the gateway's
`/model/info` by the consumer (the server), kept out of the engine. EPIC th-1cc9fa.
