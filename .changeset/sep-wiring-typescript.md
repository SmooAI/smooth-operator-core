---
'@smooai/smooth-operator-core': patch
---

TypeScript engine: wire the SEP extension host into the agent loop (`AgentOptions.extensions`) — the TS sibling of Rust's `Agent::with_extension_host` and the Go seam from #112. Eager extension tools merge in as ordinary (visible, dispatched, permission-gated) tools; deferred ones stay hidden until `tool_search` promotes them; the `tool_call` hook folds over every pending call before dispatch (veto reasons surface to the model, argument rewrites apply); and `turn_start` / `message_end` / `turn_end` events fan out on every exit — budget-exceeded and max-iteration included — in BOTH `run` and `runStream`. The seam is structural (`ExtensionHooks`), so the concrete `ExtensionHost` satisfies it with no import cycle, pinned by a compile-time assertion. No host configured ⇒ byte-identical behavior.
