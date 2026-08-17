---
"@smooai/smooth-operator-core": patch
---

fix(dotnet): record usage and cost on `RunStreamingAsync` — and make `Budget` actually stop a streamed turn

`SmoothAgent.RunStreamingAsync` declared no `UsageDetails` and no `CostTracker`.
It folded each iteration's updates with `updates.ToChatResponse()` — which
materializes both `Usage` and `ModelId` — and then threw them away, so a streamed
turn reported no tokens, no cost, and, worst of all, **`AgentOptions.Budget` was
silently inert**: a runaway streaming turn could not be stopped by its own spend
ceiling. `RunAsync` had done all three since the cost work landed; only the
streaming path was missing them.

The streaming loop now mirrors `RunAsync` exactly: `Accumulate` the usage,
`RecordWithGatewayCost` (so the gateway's authoritative cost still wins over the
local pricing table), and break on `ExceedsBudget`.

**New public API — `SmoothAgent.LastRunResponse`.** `RunStreamingAsync` returns
`IAsyncEnumerable<ChatResponseUpdate>`, which has nowhere to hang a turn total,
so this property is C#'s stand-in for the terminal event the sibling engines emit
on the stream itself (Rust `AgentEvent::Completed`, Go/Python/TypeScript `done`,
all carrying `cost_usd` and the token totals). It is null until the stream is
fully enumerated, is reset when a new streaming turn begins — so a stale total
can never be mistaken for a fresh one — and `RunAsync` does not touch it.

Downstream: `SmooAI/smooth-operator`'s C# server sums `UsageContent` chunks by
hand and then hardcodes `costUsd: 0` on `eventual_response.usage`. It can now
read a real number from the engine instead.
