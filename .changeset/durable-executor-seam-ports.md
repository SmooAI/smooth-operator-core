---
"@smooai/smooth-operator-core": patch
---

feat(go,ts,python,dotnet): port the durable-execution seam (ADR-030) to all four ports

The Rust reference has carried the ADR-030 seam for a while — `AgentExecutor` (where
and how a turn runs) over `AgentActivities` + `drive_turn` (the side-effecting model
and tool calls, split from the deterministic loop that sequences them). The four ports
had neither, so a durable backend had nowhere to plug in outside Rust.

Each port now ships both halves, in its own idiom:

- **Go** — `AgentExecutor` / `InProcessExecutor` / `AgentActivities` / `DriveTurn` /
  `InProcessActivities` in `go/core/executor.go`.
- **TypeScript** — the same names from `@smooai/smooth-operator-core` (new
  `src/executor.ts`, re-exported from the barrel).
- **Python** — `AgentExecutor` / `InProcessExecutor` / `AgentActivities` /
  `drive_turn` / `InProcessActivities` from `smooth_operator_core`.
- **.NET** — `IAgentExecutor` / `InProcessExecutor` / `IAgentActivities` /
  `AgentTurn.DriveTurnAsync` / `InProcessActivities`.

Nothing changes for existing code. Every in-process executor is a verbatim delegation
to the agent's existing run entry point, so a consumer that never mentions an executor
gets byte-for-byte the turn it got before, and each port carries the Rust
in-process-identical parity test to keep it that way. `DriveTurn` reproduces the Rust
loop statement for statement — model call, assistant-message push condition, tool
results paired to their calls, iteration bound treated as a stop rather than an error —
so a durable backend and the inline path stay the same loop.

Two deliberate divergences from Rust, both documented at their definition: the default
iteration bound tracks each port's own agent default (8 in TS/Python/.NET, 20 in Go)
rather than Rust's 50, so introducing the seam can't change how long a turn may run;
and the assistant-push condition drops Rust's `reasoning_content` arm in the ports
whose model response carries no such field.

No new dependencies in any port. A Temporal-backed backend stays out of the engine
package by design — each port carries a `TODO(ADR-030)` naming the separate opt-in
package it belongs in, mirroring the Rust `smooth-operator-temporal` crate behind its
off-by-default `temporal` cargo feature.
