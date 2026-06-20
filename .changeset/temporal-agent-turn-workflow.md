---
'@smooai/smooth-operator-core-monorepo': minor
---

Wire the real Temporal-backed agent turn (SMOODEV-1974, ADR-030): `AgentTurnWorkflow` drives the engine's `drive_turn` unchanged, proven end to end against an ephemeral Temporal dev server.

Engine (`smooth-operator-core`):

- Relax `AgentActivities` to `?Send` (drop the `Send + Sync` bound, non-`Send` futures). A Temporal workflow-backed implementation drives the single-threaded, `!Send` `WorkflowContext` (`Rc<RefCell<…>>` internally), so requiring `Send` would make `drive_turn` uncallable from workflow code. The in-process path awaits `drive_turn` directly (never across threads), so it is unaffected — all in-process tests pass unchanged.

Temporal crate (`smooth-operator-temporal`, feature `temporal`):

- Real `model_call` / `tool_invoke` activities backed by `LlmProvider` + `ToolRegistry`, supplied via a process-global installed at worker startup with `init_engine(EngineHandles { llm, tools })`. An uninitialized worker fails activities loudly (non-retryable), never silently no-ops.
- `WorkflowAgentActivities` adapter: implements the engine's `AgentActivities` by scheduling the activities on a `WorkflowContext`.
- `AgentTurnWorkflow` (+ serializable `AgentTurnInput`): seeds the conversation and calls `drive_turn` over the adapter, returning the full `Conversation`. **One loop, two backends** — the durable path is literally the in-process loop.
- New ephemeral-dev-server integration test runs a **real agent turn** (mock model via `init_engine`) through the workflow → `model_call` activity → `drive_turn` → returned conversation, asserting the scripted reply and single model call. Self-skips offline.

Next: per-step retry tuning, then HITL-via-signals and durable timers layer onto this workflow.
