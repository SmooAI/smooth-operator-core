---
'@smooai/smooth-operator-core': patch
---

feat(temporal): the Rust client-side `TemporalExecutor` — the reference language catches up with its four ports (ADR-030, th-db0816)

The Aug 17 I+Q sweep gave Go, TypeScript, Python and .NET each a client-side
Temporal `AgentExecutor`; Rust — the reference — still had none, so
`smooth-operator-server`'s per-turn executor seam had nothing to inject.
`smooai-smooth-operator-temporal` (feature `temporal`) now ships
`TemporalExecutor` + `TemporalExecutorOptions`: it implements the core
`AgentExecutor` trait by starting `AgentTurnWorkflow` and awaiting its
`Conversation`, with the same engine-handle split as the ports (the worker
holds model/tool implementations via `init_engine`; the executor holds the turn
configuration — system prompt, tool schemas, approval/wait policy).

Shared ADR-030 gaps are reproduced deliberately, not papered over:
`execute_streaming` yields no token deltas (a workflow's progress is its
history, not a live channel) and emits a single terminal `Completed` whose
usage/cost carry honest `cost_estimated` / `usage_estimated` flags; the turn
seeds from executor options only (no prior-thread history injection yet).
Durable HITL rides the existing `approve_tool` / `deny_tool` workflow signals
through an SDK workflow handle.

New skip-gated e2e (`tests/executor_e2e.rs`), verified green against a real
ephemeral Temporal dev server: a consumer holding only `Arc<dyn AgentExecutor>`
drives durable turns through both trait methods, the executor's configuration
reaches the model, and the streaming path emits exactly the terminal event.
