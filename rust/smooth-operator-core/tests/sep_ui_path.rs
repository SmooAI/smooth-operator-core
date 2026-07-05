//! The SEP `ui/request` seam on the live wire: an extension (the `sep-echo-peer`
//! in `SEP_ECHO_UI=1` mode) sends an ext→host `ui/request` from inside a
//! `tool/execute`, echoing the `ui_capabilities` the host declared at
//! `initialize` into the confirm prompt. A capable host answers it; a headless
//! host replies -32001 NoUI. This proves both the ui round-trip and that
//! `ui_capabilities` are threaded through the handshake.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use serde_json::{json, Value};
use smooth_operator_core::extension::protocol::{codes, method, RpcError};
use smooth_operator_core::extension::{ExtensionProcess, InboundHandler, SpawnSpec};

fn ui_peer_spec() -> SpawnSpec {
    SpawnSpec {
        command: env!("CARGO_BIN_EXE_sep-echo-peer").to_string(),
        args: vec![],
        env: HashMap::from([("SEP_ECHO_UI".to_string(), "1".to_string())]),
        cwd: None,
        sha256: None,
        sandbox: None,
    }
}

fn initialize_params(ui_capabilities: &[&str]) -> Value {
    json!({
        "protocol_version": 1,
        "host": { "name": "test-host", "version": "0.0.0" },
        "workspace": { "root": "/ws", "trusted": true },
        "mode": "tui",
        "ui_capabilities": ui_capabilities,
    })
}

fn tool_execute_params(call_id: &str) -> Value {
    json!({ "call_id": call_id, "tool": "say", "arguments": {}, "context": { "token": "t", "tier": "command" } })
}

/// A host delegate that answers `ui/request` confirms with a fixed verdict and
/// records the prompt it was asked (so the test can assert the caps were
/// threaded into it).
struct ConfirmingUiHost {
    verdict: bool,
    seen_prompt: Arc<Mutex<Option<String>>>,
}

#[async_trait]
impl InboundHandler for ConfirmingUiHost {
    async fn handle_request(&self, method_name: &str, params: Value) -> Result<Value, RpcError> {
        if method_name == method::UI_REQUEST {
            let prompt = params.get("prompt").and_then(Value::as_str).unwrap_or_default().to_string();
            *self.seen_prompt.lock().expect("prompt lock") = Some(prompt);
            return Ok(json!({ "confirmed": self.verdict }));
        }
        Err(RpcError::new(codes::METHOD_NOT_FOUND, format!("method not found: {method_name}")))
    }
}

#[tokio::test]
async fn ui_request_round_trips_and_threads_capabilities() {
    let seen_prompt = Arc::new(Mutex::new(None));
    let handler = Arc::new(ConfirmingUiHost {
        verdict: true,
        seen_prompt: Arc::clone(&seen_prompt),
    });
    let proc = Arc::new(ExtensionProcess::spawn(ui_peer_spec(), handler).expect("spawn ui peer"));

    // Handshake advertises two renderable ui kinds; the peer echoes them back.
    proc.request(method::INITIALIZE, initialize_params(&["confirm", "select"]), Duration::from_secs(5))
        .await
        .expect("initialize");

    let result = proc
        .request(method::TOOL_EXECUTE, tool_execute_params("c1"), Duration::from_secs(5))
        .await
        .expect("tool/execute");

    // The host answered confirmed=true, and the peer surfaced it.
    assert_eq!(result["content"], "confirmed=true", "tool should carry the host's confirm verdict");
    // The prompt the host saw carries the ui_capabilities it declared — proof
    // they were threaded through `initialize` to the extension.
    let prompt = seen_prompt.lock().expect("prompt lock").clone().expect("host should have been asked");
    assert_eq!(prompt, "caps=confirm,select", "ui_capabilities must reach the extension via initialize");
}

/// A headless host: every `ui/request` is answered -32001 NoUI, exactly like
/// the engine's `DefaultHostDelegate`.
struct HeadlessUiHost;

#[async_trait]
impl InboundHandler for HeadlessUiHost {
    async fn handle_request(&self, method_name: &str, _params: Value) -> Result<Value, RpcError> {
        if method_name == method::UI_REQUEST {
            return Err(RpcError::new(codes::NO_UI, "no UI available (headless host)"));
        }
        Err(RpcError::new(codes::METHOD_NOT_FOUND, format!("method not found: {method_name}")))
    }
}

#[tokio::test]
async fn headless_host_answers_no_ui() {
    // A headless host replies -32001 NoUI to ui/request; the peer surfaces
    // `error:-32001` instead of a verdict.
    let proc = Arc::new(ExtensionProcess::spawn(ui_peer_spec(), Arc::new(HeadlessUiHost)).expect("spawn ui peer"));
    proc.request(method::INITIALIZE, initialize_params(&[]), Duration::from_secs(5))
        .await
        .expect("initialize");

    let result = proc
        .request(method::TOOL_EXECUTE, tool_execute_params("c1"), Duration::from_secs(5))
        .await
        .expect("tool/execute");

    assert_eq!(
        result["content"],
        format!("confirmed=error:{}", codes::NO_UI),
        "headless host must answer NoUI (-32001)"
    );
}
