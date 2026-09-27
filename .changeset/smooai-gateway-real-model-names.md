---
"@smooai/smooth-operator-core": minor
---

feat(all): the `smooai-gateway` preset emits model names the gateway actually serves (SMOODEV-3342)

The Smoo AI Gateway (`llm.smoo.ai`) removed the semantic `smooth-*` aliases
(`smooth-coding`, `smooth-judge`, `smooth-default`, …) in SMOODEV-1793 and now
rejects them with HTTP 400 `Invalid model name`. Every slot of
`Preset::SmoaiGateway`, and `ProviderConfig::smooai_gateway`'s default model,
still emitted those names — so a consumer who picked the *recommended* preset got
a 400 on its first call, in all five engines.

The preset now routes to concrete gateway models, following the gateway model
policy (gpt-6-luna or a Groq alias for every call):

| slot | before | after |
| --- | --- | --- |
| coding | `smooth-coding` | `gpt-6-luna` |
| reasoning | `smooth-reasoning` | `gpt-6-luna-high` |
| reviewing | `smooth-reviewing` | `gpt-6-luna-high` |
| judge | `smooth-judge` | `groq-gpt-oss-120b` |
| summarize | `smooth-summarize` | `gpt-6-luna-fast` |
| fast | `smooth-fast` | `gpt-6-luna-fast` |
| default | `smooth-default` | `gpt-6-luna` |

Because the gateway serves concrete names, moving a slot to a different model is
now a client/preset change rather than a server-side alias remap.

The OpenAI-family defaults that still pointed at `gpt-4o` move to `gpt-6-luna`:

| default | before | after |
| --- | --- | --- |
| `ProviderConfig::openai` default model | `gpt-4o` | `gpt-6-luna` |
| `ProviderConfig::openrouter` default model | `openai/gpt-4o` | `openai/gpt-6-luna` |
| `ProviderConfig::llmgateway` default model | `openai/gpt-4o` | `openai/gpt-6-luna` |
| `LlmConfig::openrouter` model (Rust only) | `openai/gpt-4o` | `openai/gpt-6-luna` |
| `Preset::OpenAI` routing (every slot) | `gpt-4o` / `gpt-4o-mini` / `o3-mini` | `gpt-6-luna` |

Provider-native defaults are unchanged, because the model must exist on that
vendor's own API: Anthropic (claude), Google (gemini), Kimi, Kimi Code and
Ollama. The OpenRouter Low Cost and LLM Gateway Low Cost routing tables are also
unchanged. The shared corpus `spec/providers/routing.json` pins the new values,
so all five engines are held to them.

**Consumer impact:** anyone relying on a preset or provider default gets the new
model. Callers that set a model explicitly (`with_model`, `SMOOTH_MODEL`, an
on-disk `providers.json` routing table) are unaffected — but an on-disk routing
table that still names a `smooth-*` alias against the Smoo AI Gateway will keep
getting 400s until it is regenerated from the preset.
