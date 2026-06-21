---
'@smooai/smooth-operator-core-monorepo': minor
---

Durable timers / self-scheduling agents (SMOODEV-1976, ADR-030).

`AgentTurnInput` gains `wait_tool: Option<String>` — the name of a built-in **durable wait** tool. When the model calls it with an integer `seconds` argument, `AgentTurnWorkflow` sleeps on a Temporal timer (`ctx.timer`) instead of dispatching a tool activity:

- The pause is recorded in workflow history, so it **survives worker restarts** and can span days — an agent can pause itself mid-turn and resume, or schedule its own follow-up.
- Handled in the workflow adapter (like the HITL gate), so the engine's `drive_turn` is unchanged; a bad/missing `seconds` argument returns a tool-error result rather than sleeping.

New ephemeral-dev-server integration test runs a turn that waits on a real (1s) timer and resumes, asserting the durable-timer result and that the wait was actually honored. Self-skips offline.

Next: per-step retry-policy tuning and the `TemporalExecutor: AgentExecutor` reconciliation.
