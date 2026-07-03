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

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct Registrations {
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<ToolRegistration>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub commands: Vec<CommandRegistration>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub flags: Vec<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub subscriptions: Vec<String>,
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
    pub const CANCEL: &str = "$/cancel";
    pub const REGISTRY_UPDATE: &str = "registry/update";
    pub const TOOLS_SET_ACTIVE: &str = "tools/set_active";
    pub const EXEC_RUN: &str = "exec/run";
    pub const UI_REQUEST: &str = "ui/request";
    pub const LOG: &str = "log";
    pub const BUS_PUBLISH: &str = "bus/publish";
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
}
