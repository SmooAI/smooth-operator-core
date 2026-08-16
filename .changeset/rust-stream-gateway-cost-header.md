---
"@smooai/smooth-operator-core": patch
---

fix(rust): read `x-litellm-response-cost` on the STREAMING paths so `cost_usd` is real

The LLM gateway (LiteLLM at llm.smoo.ai) reports per-request cost only in a
response **header** — never in the JSON body's `usage` object, and
`_hidden_params` is null. The non-streaming `chat()` / Anthropic-native paths
already parsed it via `parse_gateway_cost`, but `chat_stream()` and
`chat_anthropic_stream()` went straight from the response to `bytes_stream()`,
dropping the headers on the floor. Since every real agent turn streams, this
pinned `LlmResponse.gateway_cost_usd` at `None`, made the agent fall back to
local `ModelPricing` (which prices aliased `smooth-*` routes at $0), and that
zero propagated cleanly through `AgentEvent::Completed.cost_usd` → `TurnUsage`
→ `usage.costUsd` → the bench leaderboard, `th code`'s status bar and the
daemon spend ledger.

Both streaming paths now parse the cost header **before** consuming the body
(the only point at which headers are still readable) and emit it as a new
`StreamEvent::Cost { usd }` — the first event of the stream —
which `accumulate_stream_events` folds onto the response. Absence is tolerated:
no header leaves `gateway_cost_usd` at `None` and the local `ModelPricing`
fallback is unchanged, so nothing locks in a bogus zero. Header precedence is
shared with the non-streaming paths (`-margin-amount` beats `-original` beats
the legacy `x-litellm-response-cost`).
