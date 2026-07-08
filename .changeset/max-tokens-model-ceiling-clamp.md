---
"@smooai/smooth-operator-core": patch
---

feat(llm): clamp `max_tokens` to the model's output ceiling

`LlmClient` gains `with_model_ceiling(Option<u32>)` + `effective_max_tokens()`.
Every request now sends `min(config.max_tokens, model.max_output_tokens)` when a
ceiling is known (`None` = graceful passthrough, no behaviour change). This lets a
policy/budget `max_tokens` — which may be tuned high or resolved per-org via
`@smooai/config` limits — never exceed what the model can physically emit, which
otherwise makes a reasoning model burn its budget on `reasoning_content` and
return empty, or 400s upstream (e.g. `groq-compound`'s 8192 output cap under a
32768 budget). The ceiling is sourced from the gateway's `/model/info` by the
consumer (kept out of the published engine so it takes no git-dep / no
LiteLLM-specific HTTP). EPIC th-1cc9fa.
