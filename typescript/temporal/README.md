# @smooai/smooth-operator-temporal

Optional **Temporal-backed durable execution** for the TypeScript
[`@smooai/smooth-operator-core`](../core) agent engine (ADR-030). The TypeScript
sibling of the Rust [`smooth-operator-temporal`](../../rust/smooth-operator-temporal)
crate.

An agent turn runs as a Temporal **workflow** whose side-effects — the model call
and each tool invocation — are Temporal **activities**. The workflow drives the
engine's deterministic `driveTurn` orchestration **unchanged**, so the durable
path and the in-process path are the *same loop*. What durability buys: crash-safe
resume, durable human-in-the-loop via signals, and durable timers (an agent that
pauses itself for minutes or days and resumes).

## Why a separate package

Mirrors the Rust crate's off-by-default `temporal` cargo feature: the published
`@smooai/smooth-operator-core` stays **zero-infra** and never pulls a Temporal SDK
into a consumer's dependency tree. Only a deployment that wants durable execution
installs this package. The activity **DTO boundary** (`./dto`) carries no Temporal
dependency and is always unit-tested.

## Shape

| Export                    | Side     | Role                                                                    |
| ------------------------- | -------- | ---------------------------------------------------------------------- |
| `TemporalAgentExecutor`   | client   | The durable `AgentExecutor`; a drop-in for the engine's `InProcessExecutor`. |
| `createActivities(h)`     | worker   | Build the worker's activity object from engine handles (model + tools). |
| `agentTurnWorkflow`       | workflow | The turn workflow (registered via the `./workflows` subpath).           |
| `approveToolSignal` / `denyToolSignal` | both | Durable HITL signals.                                        |

## Testing

```sh
# Zero-infra DTO boundary unit test (always runs, no Temporal):
pnpm --filter @smooai/smooth-operator-temporal test

# Full e2e against an ephemeral Temporal dev server (health, agent turn,
# durable timer, HITL) — self-skips offline:
pnpm --filter @smooai/smooth-operator-core build
pnpm --filter @smooai/smooth-operator-temporal build
SMOOTH_TEMPORAL_E2E=1 pnpm --filter @smooai/smooth-operator-temporal test
```
