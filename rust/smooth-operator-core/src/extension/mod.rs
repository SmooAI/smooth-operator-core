//! SEP host — the engine's implementation of the Smooth Extension Protocol.
//!
//! An extension is a long-lived subprocess speaking JSON-RPC 2.0 over ndjson on
//! its stdio (identical framing to MCP stdio). The canonical wire schemas live
//! in the `smooth-operator` repo at `spec/extension/`; [`protocol`] is the Rust
//! host's typed view of that wire.
//!
//! This module is **purely additive**: nothing here runs unless a caller builds
//! an [`ExtensionHost`] and attaches it via [`crate::Agent::with_extension_host`].
//! With no host attached the agent loop behaves exactly as before.
//!
//! Layout mirrors the plan:
//! - [`protocol`] — JSON-RPC frames + typed method params/results.
//! - [`manifest`] — `extension.toml` discovery, global+project merge, `${env:VAR}`.
//! - [`process`] — one subprocess: ndjson codec, pending map, generation-guarded restart.
//! - [`host`] — [`ExtensionHost`]: hook chaining, event fanout, tool proxies, the delegate seam.
//! - [`tool_proxy`] — [`ExtensionTool`]: an extension tool as a [`crate::Tool`].

pub mod host;
pub mod manifest;
pub mod process;
pub mod protocol;
pub mod tool_proxy;

pub use host::{fold_hook_chain, DefaultHostDelegate, ExtensionHost, FoldedHook, HookStep, HookType, HostDelegate, PROTOCOL_VERSION};
pub use manifest::{discover, Capabilities, DiscoveredExtension, ExtensionManifest, Resources, RunSpec, Scope};
pub use process::{backoff_for, DefaultInboundHandler, ExtensionProcess, InboundHandler, SpawnSpec, PING_IDLE, RESTART_BACKOFFS};
pub use protocol::{
    CommandCompleteResult, CommandExecuteResult, CommandRegistration, Completion, Context, DeliverAs, HookOutcome, Message, RpcError, ShortcutRegistration,
    Tier,
};
pub use tool_proxy::ExtensionTool;

/// Canonical SEP event names the host dispatches to subscribed extensions.
/// (A stringly-typed name is the wire contract; the engine's own
/// [`crate::AgentEvent`] variants are the typed producers that map onto these.)
///
// ponytail: names, not a `SepEvent` enum — the host filters by subscription
// string and the wire carries the name verbatim; a typed superset earns its
// keep once host-level session events (session_start/compact/tree) exist.
pub mod events {
    pub const TURN_START: &str = "turn_start";
    pub const TURN_END: &str = "turn_end";
    pub const MESSAGE_START: &str = "message_start";
    pub const MESSAGE_UPDATE: &str = "message_update";
    pub const MESSAGE_END: &str = "message_end";
    // Names mirror pi's `tool_execution_*` (not `tool_call_*`) so pi extensions
    // port their event subscriptions unchanged.
    pub const TOOL_EXECUTION_START: &str = "tool_execution_start";
    pub const TOOL_EXECUTION_UPDATE: &str = "tool_execution_update";
    pub const TOOL_EXECUTION_END: &str = "tool_execution_end";
    /// Delivered when the bounded observe queue shed events. Carries `{lost: N}`.
    pub const EVENTS_LOST: &str = "events_lost";
}
