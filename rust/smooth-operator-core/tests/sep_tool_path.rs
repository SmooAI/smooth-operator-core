//! The SEP tool path on the live wire: `tool/execute` progress via
//! `tool/update` notifications and cancellation via `$/cancel`. Runs against the
//! `sep-echo-peer` reference peer in its slow mode (`SEP_ECHO_SLOW=1`), which
//! streams a progress notification and then withholds its reply until a
//! `$/cancel` arrives. Integration test so cargo defines
//! `CARGO_BIN_EXE_sep-echo-peer`.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use serde_json::{json, Value};
use smooth_operator_core::extension::protocol::method;
use smooth_operator_core::extension::{DefaultInboundHandler, ExtensionProcess, InboundHandler, SpawnSpec};

fn slow_peer_spec() -> SpawnSpec {
    SpawnSpec {
        command: env!("CARGO_BIN_EXE_sep-echo-peer").to_string(),
        args: vec![],
        env: HashMap::from([("SEP_ECHO_SLOW".to_string(), "1".to_string())]),
        cwd: None,
        sha256: None,
    }
}

fn tool_execute_params(call_id: &str) -> Value {
    json!({ "call_id": call_id, "tool": "say", "arguments": { "phrase": "hi" }, "context": { "token": "t", "tier": "command" } })
}

/// Captures the ext→host `tool/update` notifications the host would forward to
/// its progress channel.
struct CapturingHandler {
    updates: Arc<Mutex<Vec<Value>>>,
}

impl InboundHandler for CapturingHandler {
    fn handle_notification(&self, method_name: &str, params: Value) {
        if method_name == method::TOOL_UPDATE {
            self.updates.lock().expect("updates lock").push(params);
        }
    }
}

#[tokio::test]
async fn tool_update_streams_then_cancel_round_trips() {
    let updates = Arc::new(Mutex::new(Vec::new()));
    let proc = Arc::new(ExtensionProcess::spawn(slow_peer_spec(), Arc::new(CapturingHandler { updates: Arc::clone(&updates) })).expect("spawn slow peer"));

    // `tool/execute` is the FIRST request → its JSON-RPC id is 1.
    let p = Arc::clone(&proc);
    let call = tokio::spawn(async move { p.request(method::TOOL_EXECUTE, tool_execute_params("c1"), Duration::from_secs(5)).await });

    // The peer streams a progress notification before withholding its reply.
    let mut waited = 0;
    while updates.lock().expect("updates lock").is_empty() && waited < 100 {
        tokio::time::sleep(Duration::from_millis(20)).await;
        waited += 1;
    }
    let got = updates.lock().expect("updates lock").clone();
    assert!(!got.is_empty(), "expected a tool/update progress notification");
    assert_eq!(got[0]["call_id"], "c1");
    assert_eq!(got[0]["message"], "started");

    // Cancel the in-flight call; the peer answers -32800 Cancelled.
    proc.cancel(1).expect("cancel");
    let err = call.await.expect("join").expect_err("cancelled request must error");
    assert!(err.to_string().contains("-32800"), "expected a Cancelled (-32800) error, got: {err}");
}

#[tokio::test]
async fn timed_out_tool_execute_cancels_and_leaves_process_usable() {
    let proc = ExtensionProcess::spawn(slow_peer_spec(), Arc::new(DefaultInboundHandler)).expect("spawn slow peer");

    // The peer withholds its reply → the request times out, and the drop guard
    // sends `$/cancel` and clears the pending slot.
    let err = proc
        .request(method::TOOL_EXECUTE, tool_execute_params("c1"), Duration::from_millis(200))
        .await
        .expect_err("withheld reply must time out");
    assert!(err.to_string().contains("timed out"), "{err}");

    // The peer released the held call on the auto-`$/cancel`; the late -32800
    // reply lands on the now-cleared pending slot and is dropped, leaving the
    // connection healthy for the next request.
    let pong = proc.request(method::PING, json!({}), Duration::from_secs(5)).await;
    assert!(pong.is_ok(), "process should stay usable after a cancelled tool/execute: {pong:?}");
}
