---
"@smooai/smooth-operator-core": patch
---

fix(dotnet): ship a default pricing table so an unpriced model isn't silently $0

C# was the only engine with **no local pricing fallback at all**. `AgentOptions.Pricing`
starts empty and `SmoothAgent.LookupPricing` returned null for anything not in it,
so `CostTracker.Record` charged exactly $0 on every call unless the caller had
hand-registered the model. A consumer could not tell "this model is free" from
"nobody told me the price".

Go, Python and TypeScript have shipped a `DefaultPricing` / `DEFAULT_PRICING`
table for this since the cost work landed, and their `record(...)` falls back to
it when the caller passes none. C# now carries the same two entries at the same
prices, behind `ModelPricing.Default`, plus a `ModelPricing.ForModel(modelId,
overrides)` resolver that mirrors the siblings' "caller table, then default
table, then unpriced" lookup.

Precedence is unchanged and unsurprising: the gateway's authoritative
per-request cost beats everything, then an `AgentOptions.Pricing` entry, then
`ModelPricing.Default`, then unpriced.

Deliberately **not** in scope: unifying the five engines' local tables. They
genuinely disagree about coverage — Rust's substring resolver prices `gpt-4o`,
`deepseek`, `gemini-flash` and the `smooth-*` aliases but **not** Claude, while
these three price **only** Claude. Closing that needs a cited price list rather
than an inference, since a wrong estimate mis-bills silently.
