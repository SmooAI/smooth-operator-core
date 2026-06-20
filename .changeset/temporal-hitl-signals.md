---
'@smooai/smooth-operator-core-monorepo': minor
---

Durable human-in-the-loop via Temporal signals (SMOODEV-1975, ADR-030).

`AgentTurnInput` gains `approval_required_tools: Vec<String>`. When the model calls one of those tools, `AgentTurnWorkflow` blocks **durably** until an `approve_tool` / `deny_tool` signal names that tool call:

- The gate lives in the workflow adapter's `tool_invoke` (`wait_condition` on signal-set approval state), so the engine's `drive_turn` is unchanged.
- Approved → the tool runs as normal. Denied → the tool never executes; a tool-error result is surfaced so the model can react.
- The block is recorded in workflow history, so it survives worker restarts and can resolve arbitrarily later — no mid-turn connection state, exactly the HITL-over-protocol shape that was previously deferred.

New ephemeral-dev-server integration test exercises both the approve and deny paths end to end (mock model + a gated `echo` tool, resolved via real signals). Self-skips offline.

Next: durable timers / self-scheduling (the same adapter gets `ctx.timer`).
