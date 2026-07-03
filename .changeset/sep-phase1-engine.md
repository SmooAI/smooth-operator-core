---
"@smooai/smooth-operator-core": minor
---

Complete the SEP engine tool path — Phase 1.

`Agent::with_extension_host` now registers every extension tool into the agent's
`ToolRegistry` (eager via `register_arc`, deferred via the new `register_deferred_arc`)
under its dotted `<extension>.<tool>` name, so extension tools are ordinary registry tools:
visible to the LLM through `schemas()`, dispatched through `execute()`, and filtered by the
same `retain()` a server uses to enforce a per-agent `enabled_tools` allow-list — no
special-casing, and no widening of the allow-list.

`tool/execute` gains full streaming + cancellation: `tool/update` progress notifications
route through a new `HostDelegate::tool_update` seam, and a `CancelGuard` sends `$/cancel`
(and clears the pending slot) whenever an awaiting request is dropped or times out, leaving
the connection healthy for the next call. The `sep-echo-peer` reference peer gains a slow
mode that streams progress then withholds its reply until cancelled, and new integration
tests cover the LLM→extension round-trip, registry filtering, and the update/cancel wire.
