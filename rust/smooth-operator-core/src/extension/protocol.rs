//! SEP wire protocol — JSON-RPC 2.0 frames and typed method params/results.
//!
//! SEP (the Smooth Extension Protocol) is JSON-RPC 2.0 over ndjson on an
//! extension subprocess's stdio. The canonical schemas live in the
//! `smooth-operator` repo at `spec/extension/`; the types here are the Rust
//! host's view of that wire. Field names are `snake_case` to match the spec
//! exactly (Rust field names already are, so no `rename` is needed).
//!
//! A single [`Message`] type models all four JSON-RPC frame shapes (request,
//! notification, success response, error response) with the discriminating
//! fields optional — this is what lets the host parse an arbitrary inbound line
//! and classify it, and it round-trips every conformance fixture cleanly.

use serde::{Deserialize, Serialize};

use crate::llm::Usage;
use crate::tool::ToolCall;

/// JSON-RPC error codes. Standard range plus the SEP extensions documented in
/// `spec/extension/envelope.md`.
pub mod codes {
    pub const PARSE_ERROR: i64 = -32700;
    pub const INVALID_REQUEST: i64 = -32600;
    pub const METHOD_NOT_FOUND: i64 = -32601;
    pub const INVALID_PARAMS: i64 = -32602;
    pub const INTERNAL_ERROR: i64 = -32603;

    /// A hook or policy vetoed the operation.
    pub const BLOCKED: i64 = -32000;
    /// `ui/request` in a headless/uncapable frontend.
    pub const NO_UI: i64 = -32001;
    /// Extension acted beyond its granted trust.
    pub const NOT_TRUSTED: i64 = -32002;
    /// Command-tier action attempted from an event-tier context.
    pub const CONTEXT_VIOLATION: i64 = -32003;
    /// Method requires a capability the handshake did not enable.
    pub const CAPABILITY_DISABLED: i64 = -32004;
    /// Request cancelled via `$/cancel`.
    pub const CANCELLED: i64 = -32800;
}

/// A JSON-RPC id: an integer or a string. Modeled as a `serde_json::Value` so
/// both forms (and `null`, valid only on a parse-error response) round-trip
/// without a bespoke enum.
pub type Id = serde_json::Value;

/// A JSON-RPC error object.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct RpcError {
    pub code: i64,
    pub message: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub data: Option<serde_json::Value>,
}

impl RpcError {
    #[must_use]
    pub fn new(code: i64, message: impl Into<String>) -> Self {
        Self {
            code,
            message: message.into(),
            data: None,
        }
    }
}

impl std::fmt::Display for RpcError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "JSON-RPC error {}: {}", self.code, self.message)
    }
}

impl std::error::Error for RpcError {}

/// The JSON-RPC 2.0 envelope. All four frame shapes share this struct; which
/// fields are present determines the shape:
///
/// - request: `id` + `method` (+ optional `params`)
/// - notification: `method`, no `id`
/// - success response: `id` + `result`
/// - error response: `id` + `error`
///
/// `skip_serializing_if` keeps absent fields off the wire so a request never
/// carries a `result`/`error` key (the spec's `additionalProperties: false`).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Message {
    pub jsonrpc: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub id: Option<Id>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub method: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub params: Option<serde_json::Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub result: Option<serde_json::Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error: Option<RpcError>,
}

impl Message {
    /// Build a request frame.
    #[must_use]
    pub fn request(id: Id, method: impl Into<String>, params: serde_json::Value) -> Self {
        Self {
            jsonrpc: "2.0".into(),
            id: Some(id),
            method: Some(method.into()),
            params: Some(params),
            result: None,
            error: None,
        }
    }

    /// Build a notification frame (no id, no reply expected).
    #[must_use]
    pub fn notification(method: impl Into<String>, params: serde_json::Value) -> Self {
        Self {
            jsonrpc: "2.0".into(),
            id: None,
            method: Some(method.into()),
            params: Some(params),
            result: None,
            error: None,
        }
    }

    /// Build a success response frame echoing `id`.
    #[must_use]
    pub fn success(id: Id, result: serde_json::Value) -> Self {
        Self {
            jsonrpc: "2.0".into(),
            id: Some(id),
            method: None,
            params: None,
            result: Some(result),
            error: None,
        }
    }

    /// Build an error response frame echoing `id`.
    #[must_use]
    pub fn error_response(id: Option<Id>, error: RpcError) -> Self {
        Self {
            jsonrpc: "2.0".into(),
            id,
            method: None,
            params: None,
            result: None,
            error: Some(error),
        }
    }

    /// True when this frame is a request (has both `id` and `method`).
    #[must_use]
    pub fn is_request(&self) -> bool {
        self.id.is_some() && self.method.is_some()
    }

    /// True when this frame is a notification (has `method`, no `id`).
    #[must_use]
    pub fn is_notification(&self) -> bool {
        self.id.is_none() && self.method.is_some()
    }

    /// True when this frame is a response (has `id`, no `method`).
    #[must_use]
    pub fn is_response(&self) -> bool {
        self.method.is_none() && self.id.is_some()
    }
}

// ---------------------------------------------------------------------------
// The two-tier dispatch context.
// ---------------------------------------------------------------------------

/// Whether a dispatch may only observe (`Event`) or may mutate the session
/// (`Command`). Session-mutating ext→host actions require `Command`.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Tier {
    Event,
    Command,
}

/// The dispatch context carried by every host→ext event/hook/tool/command.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Context {
    pub token: String,
    pub tier: Tier,
}

// ---------------------------------------------------------------------------
// initialize
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct HostInfo {
    pub name: String,
    pub version: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct WorkspaceInfo {
    pub root: String,
    pub trusted: bool,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct SessionInfo {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub id: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct InitializeParams {
    pub protocol_version: u32,
    pub host: HostInfo,
    pub workspace: WorkspaceInfo,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub session: Option<SessionInfo>,
    pub mode: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub ui_capabilities: Vec<String>,
    /// Parsed values for the flags the extension declares (name → value). A host
    /// with a CLI surface fills this; hosts without one send it empty. The
    /// extension reads its flag values here at startup.
    #[serde(default, skip_serializing_if = "serde_json::Map::is_empty")]
    pub flags: serde_json::Map<String, serde_json::Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub capabilities_enabled: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ExtensionInfo {
    pub name: String,
    pub version: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ToolRegistration {
    pub name: String,
    pub description: String,
    pub parameters: serde_json::Value,
    #[serde(default)]
    pub deferred: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct CommandRegistration {
    pub name: String,
    pub description: String,
}

/// A keyboard shortcut an extension binds to one of its commands. Frontends
/// that have a key surface (the TUI) honor these; headless hosts ignore them.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ShortcutRegistration {
    /// A human-typed chord, e.g. `ctrl+p` or `f2`. The frontend parses it.
    pub key: String,
    /// The registered command name this chord invokes (no leading `/`).
    pub command: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct Registrations {
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<ToolRegistration>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub commands: Vec<CommandRegistration>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub flags: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub shortcuts: Vec<ShortcutRegistration>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub subscriptions: Vec<String>,
    /// LLM providers the extension contributes to the host's model surface
    /// (Phase 7). Declarative: name + endpoint + models; the host proxies
    /// `provider/complete` back to the extension at the [`crate::llm_provider::LlmProvider`]
    /// seam. Also carried on `registry/update` for runtime registration.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub providers: Vec<ProviderRegistration>,
}

// ---------------------------------------------------------------------------
// provider registration (Phase 7)
// ---------------------------------------------------------------------------

/// One model an extension-registered provider exposes.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ProviderModel {
    /// Model id the host passes back in `provider/complete` (`model`).
    pub id: String,
    /// Human-facing label for pickers (`th cast models`, model pickers).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub display_name: Option<String>,
}

/// A provider an extension contributes. Declarative — the extension owns the
/// actual request/stream, reached over `provider/complete`; these fields let the
/// host present the provider in its model surface and mediate auth.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ProviderRegistration {
    /// Provider name, unique within the host's merged surface (`<ext>` namespacing
    /// is applied by the host, mirroring the tool `<ext>.<tool>` convention).
    pub name: String,
    /// Informational upstream base URL (the extension does the real call; the host
    /// surfaces this for diagnostics). `None` when the extension keeps it private.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub base_url: Option<String>,
    /// Env var the extension reads its API key from. Purely informational to the
    /// host (the extension resolves it in its own process). `None` for OAuth-only
    /// providers.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub api_key_env: Option<String>,
    /// Whether the extension implements `provider/oauth_login` + `oauth_refresh`.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub oauth: bool,
    /// The models this provider exposes.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub models: Vec<ProviderModel>,
}

// ---------------------------------------------------------------------------
// provider/complete + provider/delta (host↔ext, proxied streaming)
// ---------------------------------------------------------------------------

/// Host→ext `provider/complete` request: run one LLM completion through the
/// extension. `messages`/`tools` are opaque JSON — the serialized engine
/// [`Message`](crate::conversation::Message)/[`ToolSchema`](crate::tool::ToolSchema)
/// (the engine is the single source of truth for their shape). When `stream` is
/// true the extension emits `provider/delta` notifications keyed by `request_id`
/// while it works, then replies with the final [`ProviderCompleteResult`].
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ProviderCompleteParams {
    /// Correlates the `provider/delta` notification stream with this request.
    pub request_id: String,
    /// The registered provider name (bare, as the extension declared it).
    pub provider: String,
    /// Model id within the provider.
    pub model: String,
    /// Serialized conversation messages (engine `Message` shape).
    pub messages: Vec<serde_json::Value>,
    /// Serialized tool schemas offered to the model (engine `ToolSchema` shape).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<serde_json::Value>,
    /// Ask the extension to stream `provider/delta` notifications.
    #[serde(default)]
    pub stream: bool,
    /// Structured-output JSON-schema response format, if constrained.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub response_format: Option<serde_json::Value>,
    /// Reasoning/thinking level for reasoning-capable models (e.g. `off`, `low`,
    /// `medium`, `high`, or a provider-specific token). `None` = provider default.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub thinking: Option<String>,
}

/// The final reply to a `provider/complete` request. Maps directly onto the
/// engine's [`LlmResponse`](crate::llm::LlmResponse).
// No `PartialEq`: `ToolCall`/`Usage` don't implement it. Tests compare fields.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct ProviderCompleteResult {
    #[serde(default)]
    pub content: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tool_calls: Vec<ToolCall>,
    #[serde(default = "default_finish_reason")]
    pub finish_reason: String,
    #[serde(default)]
    pub usage: Usage,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reasoning_content: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub resolved_model: Option<String>,
}

fn default_finish_reason() -> String {
    "stop".to_string()
}

/// Ext→host `provider/delta` notification: one streaming chunk for an in-flight
/// `provider/complete`, keyed by `request_id`. `event` is a serialized engine
/// [`StreamEvent`](crate::llm::StreamEvent) (`{"type":"Delta","content":"…"}` …).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ProviderDeltaParams {
    pub request_id: String,
    pub event: serde_json::Value,
}

// ---------------------------------------------------------------------------
// provider/oauth_login + provider/oauth_refresh (host→ext, request)
// ---------------------------------------------------------------------------

/// Host→ext `provider/oauth_login` / `provider/oauth_refresh` request. The
/// extension runs the auth handshake, driving any user interaction (open URL,
/// prompt for a code) back through the existing `ui/*` surface, and returns the
/// resulting [`ProviderCredentials`]. `refresh_token` is set only on refresh.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ProviderOAuthParams {
    pub provider: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub refresh_token: Option<String>,
}

/// Credentials an extension's OAuth handshake produced. Opaque to the host beyond
/// persistence — the extension consumes them on subsequent `provider/complete`
/// calls. Every field optional so an api-key exchange and a full OAuth bundle
/// both fit.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct ProviderCredentials {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub api_key: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub access_token: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub refresh_token: Option<String>,
    /// Absolute expiry, unix seconds. `None` = non-expiring / unknown.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub expires_at: Option<i64>,
    /// Provider-specific extras (scopes, token_type, …).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub extra: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct InitializeResult {
    pub protocol_version: u32,
    pub extension: ExtensionInfo,
    #[serde(default)]
    pub registrations: Registrations,
}

// ---------------------------------------------------------------------------
// hook
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct HookParams {
    pub hook: String,
    pub context: Context,
    pub input: serde_json::Value,
}

/// An extension's reply to a `hook`. Serializes tagged by `action`.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(tag = "action", rename_all = "snake_case")]
pub enum HookOutcome {
    /// Proceed unchanged.
    Continue,
    /// Veto the intercepted operation.
    Block {
        #[serde(default, skip_serializing_if = "Option::is_none")]
        reason: Option<String>,
    },
    /// Replace the intercepted value with `patch`.
    Modify { patch: serde_json::Value },
}

// ---------------------------------------------------------------------------
// tool/execute + tool/update
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ToolExecuteParams {
    pub call_id: String,
    pub tool: String,
    pub arguments: serde_json::Value,
    pub context: Context,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ToolExecuteResult {
    pub content: String,
    #[serde(default)]
    pub is_error: bool,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub details: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct ToolUpdateParams {
    pub call_id: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub progress: Option<f64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub details: Option<serde_json::Value>,
}

// ---------------------------------------------------------------------------
// $/cancel, event, log
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct CancelParams {
    pub id: Id,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct EventParams {
    pub event: String,
    /// Per-connection monotonic sequence. Absent on the out-of-band
    /// `events_lost` marker (a gap in the run is itself the loss signal).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub seq: Option<u64>,
    pub context: Context,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub payload: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct LogParams {
    pub level: String,
    pub message: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub fields: Option<serde_json::Value>,
}

// ---------------------------------------------------------------------------
// command/execute + command/complete (host→ext)
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct CommandExecuteParams {
    /// The registered command name (no leading `/`, no `<ext>.` prefix).
    pub command: String,
    /// COMMAND-tier context: a command handler may take session-mutating actions.
    pub context: Context,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub arguments: Option<serde_json::Value>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct CommandExecuteResult {
    /// Optional text surfaced back into the session by the host.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub content: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct CommandCompleteParams {
    pub command: String,
    pub context: Context,
    /// The partial argument text typed so far.
    #[serde(default)]
    pub partial: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct Completion {
    pub value: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct CommandCompleteResult {
    #[serde(default)]
    pub completions: Vec<Completion>,
}

// ---------------------------------------------------------------------------
// session/* (ext→host) — all require COMMAND tier
// ---------------------------------------------------------------------------

/// How a `session/send_user_message` is delivered relative to the current turn.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DeliverAs {
    /// Interrupt the in-flight turn with the new user message.
    Steer,
    /// Queue after the current turn completes.
    FollowUp,
    /// Deliver at the start of the next turn.
    NextTurn,
}

impl DeliverAs {
    #[must_use]
    pub fn as_str(self) -> &'static str {
        match self {
            DeliverAs::Steer => "steer",
            DeliverAs::FollowUp => "follow_up",
            DeliverAs::NextTurn => "next_turn",
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct SessionSendMessageParams {
    pub context: Context,
    pub text: String,
    /// `user` or `assistant`; defaults to `assistant`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub role: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct SessionSendUserMessageParams {
    pub context: Context,
    pub text: String,
    #[serde(default = "default_deliver_as")]
    pub deliver_as: DeliverAs,
}

fn default_deliver_as() -> DeliverAs {
    DeliverAs::FollowUp
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct SessionAppendEntryParams {
    pub context: Context,
    /// An opaque transcript entry, persisted but NOT sent to the model.
    pub entry: serde_json::Value,
}

/// `session/set_model` (Phase 7): switch the active model, optionally to an
/// extension-registered provider, and optionally set a reasoning/thinking level.
/// Command-tier + current-epoch, like every session action.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct SessionSetModelParams {
    pub context: Context,
    /// Model id to switch to.
    pub model: String,
    /// Provider name when the model belongs to an extension-registered provider.
    /// `None` selects the model on the host's default/native provider surface.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub provider: Option<String>,
    /// Reasoning/thinking level (see [`ProviderCompleteParams::thinking`]).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub thinking: Option<String>,
}

/// SEP method names, centralized so the host and tests never spell one wrong.
pub mod method {
    pub const INITIALIZE: &str = "initialize";
    pub const SHUTDOWN: &str = "shutdown";
    pub const PING: &str = "ping";
    pub const EVENT: &str = "event";
    pub const HOOK: &str = "hook";
    pub const TOOL_EXECUTE: &str = "tool/execute";
    pub const TOOL_UPDATE: &str = "tool/update";
    pub const COMMAND_EXECUTE: &str = "command/execute";
    pub const COMMAND_COMPLETE: &str = "command/complete";
    pub const CANCEL: &str = "$/cancel";
    pub const REGISTRY_UPDATE: &str = "registry/update";
    pub const TOOLS_SET_ACTIVE: &str = "tools/set_active";
    pub const EXEC_RUN: &str = "exec/run";
    pub const UI_REQUEST: &str = "ui/request";
    pub const LOG: &str = "log";
    pub const BUS_PUBLISH: &str = "bus/publish";
    pub const SESSION_SEND_MESSAGE: &str = "session/send_message";
    pub const SESSION_SEND_USER_MESSAGE: &str = "session/send_user_message";
    pub const SESSION_APPEND_ENTRY: &str = "session/append_entry";
    pub const SESSION_SET_MODEL: &str = "session/set_model";
    // provider/* (Phase 7)
    pub const PROVIDER_COMPLETE: &str = "provider/complete";
    pub const PROVIDER_DELTA: &str = "provider/delta";
    pub const PROVIDER_OAUTH_LOGIN: &str = "provider/oauth_login";
    pub const PROVIDER_OAUTH_REFRESH: &str = "provider/oauth_refresh";
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn roundtrip<T>(value: &T) -> T
    where
        T: Serialize + for<'de> Deserialize<'de>,
    {
        let s = serde_json::to_string(value).expect("serialize");
        serde_json::from_str(&s).expect("deserialize")
    }

    #[test]
    fn message_classification() {
        let req = Message::request(json!(1), "ping", json!({}));
        assert!(req.is_request() && !req.is_notification() && !req.is_response());

        let note = Message::notification("event", json!({}));
        assert!(note.is_notification() && !note.is_request());

        let ok = Message::success(json!(1), json!({}));
        assert!(ok.is_response() && !ok.is_request());

        let err = Message::error_response(Some(json!(1)), RpcError::new(codes::BLOCKED, "no"));
        assert!(err.is_response());
    }

    #[test]
    fn request_frame_omits_result_and_error() {
        let req = Message::request(json!(7), "tool/execute", json!({"x": 1}));
        let s = serde_json::to_string(&req).expect("serialize");
        assert!(!s.contains("result"), "{s}");
        assert!(!s.contains("error"), "{s}");
        assert!(s.contains("\"jsonrpc\":\"2.0\""));
        assert!(s.contains("\"method\":\"tool/execute\""));
    }

    #[test]
    fn notification_has_no_id() {
        // Use params without an `id` key so the top-level-id check is meaningful.
        let note = Message::notification("event", json!({"event": "turn_start"}));
        assert!(note.id.is_none());
        let s = serde_json::to_string(&note).expect("serialize");
        assert!(!s.contains("\"id\""), "{s}");
    }

    #[test]
    fn message_roundtrips_all_shapes() {
        for m in [
            Message::request(json!("abc"), "initialize", json!({})),
            Message::notification("log", json!({"level": "info", "message": "hi"})),
            Message::success(json!(2), json!({"ok": true})),
            Message::error_response(Some(json!(2)), RpcError::new(codes::CANCELLED, "cancelled")),
            Message::error_response(None, RpcError::new(codes::PARSE_ERROR, "bad json")),
        ] {
            assert_eq!(roundtrip(&m), m);
        }
    }

    #[test]
    fn initialize_params_roundtrip() {
        let p = InitializeParams {
            protocol_version: 1,
            host: HostInfo {
                name: "smooth-operator-core".into(),
                version: "0.15.0".into(),
            },
            workspace: WorkspaceInfo {
                root: "/ws".into(),
                trusted: true,
            },
            session: Some(SessionInfo { id: Some("s1".into()) }),
            mode: "headless".into(),
            ui_capabilities: vec!["confirm".into()],
            flags: serde_json::Map::new(),
            capabilities_enabled: Some(json!({"tools": true})),
        };
        assert_eq!(roundtrip(&p), p);
    }

    #[test]
    fn initialize_result_roundtrip() {
        let r = InitializeResult {
            protocol_version: 1,
            extension: ExtensionInfo {
                name: "echo".into(),
                version: "0.1.0".into(),
            },
            registrations: Registrations {
                tools: vec![ToolRegistration {
                    name: "say".into(),
                    description: "Echo a phrase back.".into(),
                    parameters: json!({"type": "object"}),
                    deferred: false,
                }],
                subscriptions: vec!["turn_start".into()],
                ..Default::default()
            },
        };
        assert_eq!(roundtrip(&r), r);
    }

    #[test]
    fn hook_outcome_variants_serialize_by_action() {
        assert_eq!(serde_json::to_value(HookOutcome::Continue).unwrap(), json!({"action": "continue"}));
        assert_eq!(
            serde_json::to_value(HookOutcome::Block { reason: Some("nope".into()) }).unwrap(),
            json!({"action": "block", "reason": "nope"})
        );
        assert_eq!(serde_json::to_value(HookOutcome::Block { reason: None }).unwrap(), json!({"action": "block"}));
        assert_eq!(
            serde_json::to_value(HookOutcome::Modify { patch: json!({"a": 1}) }).unwrap(),
            json!({"action": "modify", "patch": {"a": 1}})
        );
    }

    #[test]
    fn hook_outcome_parses_from_wire() {
        let c: HookOutcome = serde_json::from_value(json!({"action": "continue"})).unwrap();
        assert_eq!(c, HookOutcome::Continue);
        let m: HookOutcome = serde_json::from_value(json!({"action": "modify", "patch": {}})).unwrap();
        assert!(matches!(m, HookOutcome::Modify { .. }));
    }

    #[test]
    fn hook_outcome_rejects_unknown_action() {
        let r: Result<HookOutcome, _> = serde_json::from_value(json!({"action": "bogus"}));
        assert!(r.is_err());
    }

    #[test]
    fn tier_serializes_snake_case() {
        assert_eq!(serde_json::to_value(Tier::Command).unwrap(), json!("command"));
        assert_eq!(serde_json::to_value(Tier::Event).unwrap(), json!("event"));
    }

    #[test]
    fn tool_execute_roundtrip() {
        let p = ToolExecuteParams {
            call_id: "c1".into(),
            tool: "say".into(),
            arguments: json!({"phrase": "hi"}),
            context: Context {
                token: "t".into(),
                tier: Tier::Command,
            },
        };
        assert_eq!(roundtrip(&p), p);
        let r = ToolExecuteResult {
            content: "hi".into(),
            is_error: false,
            details: None,
        };
        assert_eq!(roundtrip(&r), r);
    }

    #[test]
    fn rpc_error_is_std_error() {
        let e = RpcError::new(codes::NO_UI, "headless");
        assert_eq!(e.to_string(), "JSON-RPC error -32001: headless");
    }

    // -------- Phase 7: provider registration + proxied streaming + set_model --

    #[test]
    fn provider_registration_roundtrips_in_registrations() {
        let regs = Registrations {
            providers: vec![ProviderRegistration {
                name: "corporate-proxy".into(),
                base_url: Some("https://llm.internal.example/v1".into()),
                api_key_env: Some("CORP_LLM_KEY".into()),
                oauth: true,
                models: vec![
                    ProviderModel {
                        id: "corp-gpt-4o".into(),
                        display_name: Some("Corporate GPT-4o".into()),
                    },
                    ProviderModel {
                        id: "corp-fast".into(),
                        display_name: None,
                    },
                ],
            }],
            ..Default::default()
        };
        assert_eq!(roundtrip(&regs), regs);
    }

    #[test]
    fn provider_registration_omits_defaults_on_the_wire() {
        // A minimal provider (no base_url/api_key_env, oauth=false) serializes lean.
        let p = ProviderRegistration {
            name: "p".into(),
            base_url: None,
            api_key_env: None,
            oauth: false,
            models: vec![],
        };
        let s = serde_json::to_string(&p).unwrap();
        assert!(!s.contains("oauth"), "false oauth must be omitted: {s}");
        assert!(!s.contains("base_url"), "{s}");
        assert!(!s.contains("models"), "empty models omitted: {s}");
    }

    #[test]
    fn provider_complete_params_roundtrip() {
        let p = ProviderCompleteParams {
            request_id: "req-1".into(),
            provider: "corporate-proxy".into(),
            model: "corp-gpt-4o".into(),
            messages: vec![json!({"role": "user", "content": "hi"})],
            tools: vec![json!({"name": "search", "description": "", "parameters": {}})],
            stream: true,
            response_format: Some(json!({"type": "json_schema"})),
            thinking: Some("high".into()),
        };
        assert_eq!(roundtrip(&p), p);
    }

    #[test]
    fn provider_complete_result_deserializes_with_defaults() {
        // Only `content` present → finish_reason defaults to "stop", usage default.
        let r: ProviderCompleteResult = serde_json::from_value(json!({"content": "hello"})).unwrap();
        assert_eq!(r.content, "hello");
        assert_eq!(r.finish_reason, "stop");
        assert!(r.tool_calls.is_empty());
        assert_eq!(r.usage.total_tokens, 0);
    }

    #[test]
    fn provider_delta_params_roundtrip() {
        let d = ProviderDeltaParams {
            request_id: "req-1".into(),
            event: json!({"type": "Delta", "content": "hel"}),
        };
        assert_eq!(roundtrip(&d), d);
    }

    #[test]
    fn provider_credentials_all_optional() {
        // Empty credentials serialize to `{}` — every field is skip-if-none.
        let empty = ProviderCredentials::default();
        assert_eq!(serde_json::to_value(&empty).unwrap(), json!({}));
        let full = ProviderCredentials {
            api_key: Some("sk-x".into()),
            refresh_token: Some("rt".into()),
            expires_at: Some(1_800_000_000),
            ..Default::default()
        };
        assert_eq!(roundtrip(&full), full);
    }

    #[test]
    fn session_set_model_params_roundtrip() {
        let p = SessionSetModelParams {
            context: Context {
                token: "epoch-1".into(),
                tier: Tier::Command,
            },
            model: "corp-gpt-4o".into(),
            provider: Some("corporate-proxy".into()),
            thinking: Some("medium".into()),
        };
        assert_eq!(roundtrip(&p), p);
    }
}
