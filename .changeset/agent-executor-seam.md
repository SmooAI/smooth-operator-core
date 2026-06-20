---
'@smooai/smooth-operator-core-monorepo': minor
---

Add an `AgentExecutor` abstraction — the turn-execution backend seam (SMOODEV-1973, ADR-030).

New public API:

- `AgentExecutor` trait — `execute(&Agent, String)` and `execute_streaming(&Agent, String, UnboundedSender<AgentEvent>)`. Decides *where/how* an agent turn runs while `Agent` remains the unit of orchestration. Object-safe, so consumers can hold `Arc<dyn AgentExecutor>` and swap backends.
- `InProcessExecutor` — the default, zero-infra backend. A verbatim delegation to `Agent::run` / `Agent::run_with_channel`; introducing it changes no behavior.

This is the boundary an optional, off-by-default Temporal-backed executor (separate crate, SMOODEV-1974) plugs into to add crash-safe resume, durable human-in-the-loop via signals, and durable timers. The in-process path runs side-effects inline; a durable backend runs them as activities behind this same trait.
