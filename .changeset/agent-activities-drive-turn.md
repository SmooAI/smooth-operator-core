---
'@smooai/smooth-operator-core-monorepo': minor
---

Add the deterministic turn orchestration over a pluggable I/O surface (SMOODEV-1974, ADR-030) — the keystone of the fine-grained durable executor.

New public API (`activities` module):

- `AgentActivities` trait — the side-effecting boundary of a turn: `model_call(messages, tools) -> LlmResponse` and `tool_invoke(call) -> ToolResult`. These are exactly the steps a durable backend runs as Temporal **activities**; inputs/outputs are owned, serde-friendly values so they can cross an activity boundary.
- `drive_turn(activities, conversation, tools, policy)` — the deterministic orchestration: context window → model call → append assistant → stop-or-run-tools → append tool results → loop to `max_iterations`. Contains no I/O, wall-clock, or RNG, so the **same loop** runs inline *and* as Temporal workflow code — which is what keeps the durable path from diverging from the inline path.
- `InProcessActivities` — the zero-infra implementation, backed by the existing `LlmProvider` + `ToolRegistry` seams.
- `TurnPolicy` — the iteration bound (defaults to 50, matching `AgentConfig`).

`drive_turn` reproduces the core decision flow of `Agent::run`; a parity test asserts the two produce the same message sequence for an identical script. Inline-runtime concerns (event emission, checkpointing, compaction, budget, parallel tools, knowledge/memory injection) are deliberately out of this orchestration — a durable backend models them differently, and `Agent::run`'s convergence onto `drive_turn` is a tracked follow-up under the epic. The Temporal-backed `AgentActivities` impl (separate, feature-gated crate) is the next increment.
