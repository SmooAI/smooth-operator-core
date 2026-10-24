---
"@smooai/smooth-operator-core": minor
---

fix(rust): streamed turns get a real cost instead of $0 (SMOODEV-4057, pearl th-c420bf)

Every streamed turn on llm.smoo.ai recorded `cost_usd = 0`. LiteLLM's cost headers are flushed before the completion is priced, so on a stream they read 0 and map to `None`. The agent then fell back to the local `ModelPricing` table, which knows a handful of models and prices everything else (`gpt-6-luna`, every `smooth-*` alias) at the free tier.

- **`usage.cost` is read.** The final stream chunk (empty `choices`) and a non-streaming body can carry the gateway's own price for the call on the usage object (LiteLLM with `include_cost_in_streaming_usage`, OpenRouter always). It now becomes `gateway_cost_usd`. On a stream it supersedes the header figure, since it is sent after pricing; on a non-streaming call the header still wins.
- **Gateway list-price fallback.** When a call carries no gateway cost, the agent prices its real tokens at the gateway's published `input_cost_per_token` / `output_cost_per_token` from LiteLLM's `/model/info` before falling back to `ModelPricing`. The price book is fetched once per gateway and key and cached process-wide for an hour (a failed fetch for five minutes, so OpenAI or OpenRouter, which have no `/model/info`, cost one request per window). A model priced 0/0 counts as unpriced, not free. A list-priced cost still sets `cost_estimated`, because the gateway did not bill that figure. Calls through an injected `LlmProvider` skip the lookup.
- New public API: `cost::gateway_list_price`, `resolution::fetch_model_pricing`, `resolution::parse_model_pricing`.

Rust only. The other engines still need the same change for parity.
