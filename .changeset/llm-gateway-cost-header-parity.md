---
"@smooai/smooth-operator-core": patch
---

fix(go,ts,python,dotnet): read the gateway's cost header so `costUsd` is real, not 0

The LLM gateway reports per-request cost ONLY in a response header. Rust reads it
(core#102); the four ports never did, so every turn fell through to local
`ModelPricing` — which prices aliased `smooth-*` routes at $0. The lockstep
version bump then shipped that as "fixed" in all five engines, which is the root
of the costUsd-only-on-Rust finding.

All four ports now mirror Rust's parser exactly, same candidate list and
precedence: `x-litellm-response-cost-margin-amount` → `-original` →
`x-litellm-response-cost` → `x-response-cost` → `x-cost-usd`, taking the FIRST
NON-ZERO value.

**Absent AND zero both mean "unmeasured".** A present `0` is not locked in either
— it falls through to the next candidate, and if nothing measures, the parser
returns null so the caller uses the local estimate. That is the actual bug: a
real $0 and "the gateway didn't tell us" must stay distinct.

- **Go** — full port: it owns its HTTP client, so both paths read real headers.
  Non-streaming attaches `ChatResponse.GatewayCostUSD`; streaming reads headers
  BEFORE the SSE body is consumed (they are gone once it is) and carries the cost
  on the first `ChatChunk`. `CostTracker.RecordWithGatewayCost` prefers it.
- **TypeScript / Python / .NET** — these take an injected client (`openai` SDK,
  `IChatClient`) and have no HTTP client of their own, so they get the parser plus
  the seam the cost flows through: a `gatewayCostUsd` a wrapping client attached,
  or raw `headers` hung off the response (`.withResponse()` /
  `.with_raw_response` / `AdditionalProperties`). A client that surfaces headers
  now lands a real cost on the turn, and a native HTTP client will inherit it.
