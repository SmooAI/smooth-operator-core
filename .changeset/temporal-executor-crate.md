---
'@smooai/smooth-operator-core-monorepo': minor
---

Add the `smooth-operator-temporal` crate — optional Temporal-backed durable execution (SMOODEV-1974, ADR-030), with an ephemeral-dev-server integration harness.

- New crate `smooai-smooth-operator-temporal`. The preview Temporal SDK (`temporalio-* 0.4`) and all workflow/executor wiring are behind the **`temporal`** cargo feature (off by default), so the engine's default build pulls in no Temporal dependency and stays zero-infra.
- Always-compiled serde **DTO boundary** (`dto`): `ModelCallInput` / `ModelCallOutput` / `ToolInvokeInput`, with `LlmResponse` ⇄ DTO conversions (round-trip tested). `LlmResponse` isn't `Serialize`, so the `model_call` activity marshals the DTO and the workflow reconstructs the response.
- Feature-gated `temporal` module: a scaffold `HealthWorkflow` + `AgentTurnActivities` proving the SDK integrates in this crate.
- **Integration test against a real ephemeral Temporal dev server** — the SDK auto-downloads/caches the Temporal CLI and runs a full worker + workflow + activity round-trip in-process (no Docker, no manual install). Self-skips if the download is blocked (offline/CI), mirroring the engine's Docker-gated Postgres tests.
- CI: PR checks install `protoc` and exercise the `temporal` feature (clippy + tests) so the feature-gated code can't silently rot.

Next: the real `model_call` / `tool_invoke` activities + an `AgentTurnWorkflow` that drives the engine's `drive_turn` unchanged, then `TemporalExecutor: AgentExecutor`.
