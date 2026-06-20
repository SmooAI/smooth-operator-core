//! # smooth-operator-temporal
//!
//! Optional Temporal-backed durable execution backend for the `smooth-operator`
//! agent engine (ADR-030). An agent turn runs as a Temporal **workflow** whose
//! side-effects — the model call and each tool invocation — are Temporal
//! **activities**. The workflow drives the engine's deterministic
//! [`drive_turn`](smooth_operator_core::drive_turn) orchestration
//! unchanged, so the durable path and the in-process path are the *same loop*.
//!
//! ## Feature gating
//!
//! The preview Temporal SDK and all workflow/executor wiring live behind the
//! **`temporal`** cargo feature (off by default). Without it, this crate
//! compiles only the serde [`dto`] boundary — no `temporalio-*` dependency is
//! pulled in, so the engine's default build stays zero-infra. Enable it with:
//!
//! ```toml
//! smooai-smooth-operator-temporal = { path = "...", features = ["temporal"] }
//! ```

pub mod dto;

#[cfg(feature = "temporal")]
pub mod temporal;
