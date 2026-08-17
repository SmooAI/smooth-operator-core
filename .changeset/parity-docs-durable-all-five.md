---
'@smooai/smooth-operator-core': patch
---

Reconcile the parity docs now that the durable-execution backend ships in all five engines.

The durable-execution **backend** (Temporal) is no longer a Rust-first exception. It now ships in all five languages, each as a separate, optional per-language package that mirrors the Rust `smooth-operator-temporal` crate: the Go `go/temporal` module (#170), TS `@smooai/smooth-operator-temporal` (#168), Python `smooai-smooth-operator-temporal` (#169), and .NET `SmooAI.SmoothOperator.Temporal` (#173). Each runs an agent turn as a Temporal `AgentTurnWorkflow` with the model call and tool invocations as activities, giving crash-safe resume, durable human-in-the-loop via approve/deny signals, and a durable-wait timer, and is verified by a skip-gated e2e against a real ephemeral Temporal server. The Temporal SDK stays isolated in the optional package, so no engine pulls it into your dependency tree.

The README and `docs/Polyglot-Engines.md` now state the durable backend as in all five, and the only remaining Rust-first engine surface is the extension **sandbox / integrity hardening**. The honest ADR-030 follow-ups are kept and reframed as shared across all five (including Rust), not a Rust-vs-others gap: the durable path yields only a terminal result (no token-delta streaming) and reports `costUsd = 0` on the workflow result, and the executor seeds from agent config only — the workflow→streaming adapter bridge that closes these is still open.
