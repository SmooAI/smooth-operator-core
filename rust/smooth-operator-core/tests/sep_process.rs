//! Live SEP subprocess lifecycle, against the `sep-echo-peer` reference peer.
//! Integration test so cargo defines `CARGO_BIN_EXE_sep-echo-peer`.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use smooth_operator_core::extension::protocol::{method, Context, Tier, ToolRegistration};
use smooth_operator_core::extension::{DefaultInboundHandler, ExtensionProcess, ExtensionTool, SpawnSpec};
use smooth_operator_core::Tool;

fn peer_spec() -> SpawnSpec {
    SpawnSpec {
        command: env!("CARGO_BIN_EXE_sep-echo-peer").to_string(),
        args: vec![],
        env: HashMap::new(),
        cwd: None,
        sha256: None,
        sandbox: None,
    }
}

fn spawn_peer() -> ExtensionProcess {
    ExtensionProcess::spawn(peer_spec(), Arc::new(DefaultInboundHandler)).expect("spawn peer")
}

/// th-210910: the host scrubs its environment before spawning an extension. A
/// host-side secret must NOT leak into the child, while PATH (allow-listed) and
/// the manifest's own vars pass through. Drives the REAL spawn path end-to-end
/// and reads the child's observed env off the peer's initialize reply.
#[tokio::test]
async fn spawn_scrubs_host_secrets_but_passes_path_and_explicit_env() {
    // A secret in the host process env — the kind that must never reach an
    // extension (lethal-trifecta). Set before spawn so the child could inherit
    // it if the scrub were missing.
    std::env::set_var("AWS_SECRET_ACCESS_KEY", "th-210910-should-not-leak");

    let spec = SpawnSpec {
        command: env!("CARGO_BIN_EXE_sep-echo-peer").to_string(),
        args: vec![],
        env: HashMap::from([("SEP_ECHO_ENV".to_string(), "1".to_string())]),
        cwd: None,
        sha256: None,
        sandbox: None,
    };
    let proc = ExtensionProcess::spawn(spec, Arc::new(DefaultInboundHandler)).expect("spawn peer");
    let result = proc
        .request(method::INITIALIZE, serde_json::json!({ "protocol_version": 1 }), Duration::from_secs(5))
        .await
        .expect("initialize");

    std::env::remove_var("AWS_SECRET_ACCESS_KEY");

    let report = result.get("env_report").expect("peer reports env");
    // The host secret was SCRUBBED — the child saw no such var (reports null).
    assert!(report["AWS_SECRET_ACCESS_KEY"].is_null(), "host secret leaked into extension: {report}");
    // PATH (allow-listed) passed through so a bare-name interpreter can resolve.
    assert_eq!(report["PATH_present"], serde_json::json!(true), "PATH must pass through");
    // The manifest's explicit (SEP-protocol) var passed through.
    assert_eq!(report["SEP_ECHO_ENV"], serde_json::json!("1"), "explicit manifest env must pass through");
}

/// th-210910: the integrity gate is wired into the real spawn path — a pinned
/// hash that does not match the on-disk binary refuses to spawn.
#[test]
fn spawn_refuses_on_integrity_mismatch() {
    let spec = SpawnSpec {
        command: env!("CARGO_BIN_EXE_sep-echo-peer").to_string(),
        args: vec![],
        env: HashMap::new(),
        cwd: None,
        sha256: Some("00".repeat(32)), // deliberately wrong pin
        sandbox: None,
    };
    let err = ExtensionProcess::spawn(spec, Arc::new(DefaultInboundHandler)).expect_err("wrong pin must refuse");
    assert!(err.to_string().contains("integrity check FAILED"), "{err}");
}

#[tokio::test]
async fn spawn_handshake_request_response() {
    let proc = spawn_peer();
    let result = proc
        .request(method::INITIALIZE, serde_json::json!({"protocol_version": 1}), Duration::from_secs(5))
        .await
        .expect("initialize");
    assert_eq!(result["extension"]["name"], "echo");
    assert_eq!(result["protocol_version"], 1);
}

#[tokio::test]
async fn tool_execute_round_trips() {
    let proc = spawn_peer();
    let out = proc
        .request(
            method::TOOL_EXECUTE,
            serde_json::json!({"call_id": "c1", "tool": "say", "arguments": {"phrase": "hello sep"}, "context": {"token": "t", "tier": "command"}}),
            Duration::from_secs(5),
        )
        .await
        .expect("tool/execute");
    assert_eq!(out["content"], "hello sep");
}

#[tokio::test]
async fn unknown_method_returns_rpc_error() {
    let proc = spawn_peer();
    let err = proc.request("does/not_exist", serde_json::json!({}), Duration::from_secs(5)).await.unwrap_err();
    assert!(err.to_string().contains("-32601"), "{err}");
}

#[tokio::test]
async fn request_times_out_when_peer_silent() {
    // `sleep` never reads stdin nor writes stdout, so the request gets no
    // response and must hit the timeout path. (`cat` would echo our request
    // back, which the host then auto-answers — not a silent peer.)
    let spec = SpawnSpec {
        command: "sleep".into(),
        args: vec!["30".into()],
        env: HashMap::new(),
        cwd: None,
        sha256: None,
        sandbox: None,
    };
    let proc = ExtensionProcess::spawn(spec, Arc::new(DefaultInboundHandler)).expect("spawn");
    let err = proc.request(method::PING, serde_json::json!({}), Duration::from_millis(200)).await.unwrap_err();
    assert!(err.to_string().contains("timed out"), "{err}");
}

#[tokio::test]
async fn respawn_bumps_generation_and_recovers() {
    let proc = spawn_peer();
    assert_eq!(proc.generation(), 0);
    proc.request(method::INITIALIZE, serde_json::json!({"protocol_version": 1}), Duration::from_secs(5))
        .await
        .expect("first initialize");

    proc.respawn().expect("respawn");
    assert_eq!(proc.generation(), 1);
    assert!(proc.is_alive());

    // The fresh child answers normally.
    let out = proc
        .request(
            method::TOOL_EXECUTE,
            serde_json::json!({"call_id": "c", "tool": "say", "arguments": {"phrase": "again"}, "context": {"token": "t", "tier": "command"}}),
            Duration::from_secs(5),
        )
        .await
        .expect("post-respawn tool/execute");
    assert_eq!(out["content"], "again");
}

#[tokio::test]
async fn dead_after_child_exits() {
    let proc = spawn_peer();
    proc.shutdown(Duration::from_secs(2)).await;
    assert!(!proc.is_alive());
    let err = proc.request(method::PING, serde_json::json!({}), Duration::from_secs(1)).await.unwrap_err();
    assert!(err.to_string().contains("not alive"), "{err}");
}

#[tokio::test]
async fn ping_health_true_for_live_peer() {
    let proc = spawn_peer();
    assert!(proc.ping_health(Duration::from_secs(5)).await);
}

#[tokio::test]
async fn extension_tool_execute_forwards_to_peer() {
    let proc = Arc::new(spawn_peer());
    let reg = ToolRegistration {
        name: "say".into(),
        description: "Echo a phrase back.".into(),
        parameters: serde_json::json!({"type": "object", "properties": {"phrase": {"type": "string"}}, "required": ["phrase"]}),
        deferred: false,
    };
    let tool = ExtensionTool::new(
        "echo",
        &reg,
        proc,
        Context {
            token: "t".into(),
            tier: Tier::Command,
        },
    );
    let out = tool.execute(serde_json::json!({"phrase": "proxied"})).await.expect("execute");
    assert_eq!(out, "proxied");
}
