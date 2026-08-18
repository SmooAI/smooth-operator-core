# @smooai/smooth-operator-temporal

## 1.9.3

### Patch Changes

- Updated dependencies [418d996]
  - @smooai/smooth-operator-core@1.10.0

## 1.9.2

### Patch Changes

- Updated dependencies [60c29b9]
- Updated dependencies [9dbb8fd]
  - @smooai/smooth-operator-core@1.9.0

## 1.9.1

### Patch Changes

- Updated dependencies [f4ca614]
  - @smooai/smooth-operator-core@1.8.12

## 1.9.0

### Minor Changes

- fa2aba7: Add a Temporal-backed durable execution backend for TypeScript — the sibling of the Rust `smooth-operator-temporal` crate (ADR-030, parity item I).

  A new **optional** package, `@smooai/smooth-operator-temporal`, runs an agent turn as a Temporal **workflow** whose model call and each tool invocation are Temporal **activities**. The workflow drives the engine's deterministic `driveTurn` orchestration **unchanged**, so the durable path and the in-process path are the _same loop_ — the durable path just gets crash-safe resume, durable human-in-the-loop via `approveTool` / `denyTool` signals, and durable timers (an agent that pauses itself on a Temporal timer and resumes). `TemporalAgentExecutor` implements the engine's `AgentExecutor` interface, a drop-in for `InProcessExecutor`.

  Kept off the default path exactly like the Rust crate's off-by-default `temporal` cargo feature: the published `@smooai/smooth-operator-core` pulls in no Temporal SDK. The activity DTO boundary carries no Temporal dependency and is unit-tested without a runtime; the full e2e (health, agent turn, durable timer, HITL) runs against an ephemeral Temporal test server and self-skips offline.

  Core adds a small `./executor` subpath export so the workflow bundle can import the pure `driveTurn` loop (a type-only, zero-runtime-dependency entry point) without pulling the whole engine into Temporal's deterministic workflow sandbox.

### Patch Changes

- Updated dependencies [2561905]
- Updated dependencies [fa2aba7]
  - @smooai/smooth-operator-core@1.8.11
