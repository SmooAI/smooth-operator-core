//! SEP Phase 7 on the live wire: an extension-registered LLM provider proxied
//! through the real [`ExtensionHost`]. Drives the `sep-echo-peer` in provider
//! mode (`SEP_ECHO_PROVIDER=1`), which registers `echo-provider`, streams two
//! `provider/delta` chunks before its `provider/complete` reply, and answers the
//! OAuth methods. Integration test so cargo defines `CARGO_BIN_EXE_sep-echo-peer`.

use std::sync::Arc;

use futures_util::StreamExt;
use smooth_operator_core::conversation::Message;
use smooth_operator_core::extension::protocol::{HostInfo, WorkspaceInfo};
use smooth_operator_core::extension::{discover, DefaultHostDelegate, ExtensionHost};
use smooth_operator_core::llm::StreamEvent;

/// Write an `echo` extension manifest pointing at the reference peer in provider
/// mode.
fn write_provider_manifest(global: &std::path::Path) {
    let dir = global.join("echo");
    std::fs::create_dir_all(&dir).unwrap();
    let peer = env!("CARGO_BIN_EXE_sep-echo-peer");
    let toml =
        format!("name = \"echo\"\nversion = \"0.1.0\"\n[run]\ncommand = \"{peer}\"\nenv = {{ SEP_ECHO_PROVIDER = \"1\" }}\n[capabilities]\ntools = true\n");
    std::fs::write(dir.join("extension.toml"), toml).unwrap();
}

async fn load_host(global: &std::path::Path) -> ExtensionHost {
    let (discovered, failures) = discover(Some(global), None);
    assert!(failures.is_empty(), "discovery failures: {failures:?}");
    let (host, load_failures) = ExtensionHost::load(
        discovered,
        HostInfo {
            name: "test-host".into(),
            version: "0.0.0".into(),
        },
        WorkspaceInfo {
            root: "/ws".into(),
            trusted: true,
        },
        "headless",
        Vec::new(),
        Arc::new(DefaultHostDelegate),
    )
    .await;
    assert!(load_failures.is_empty(), "load failures: {load_failures:?}");
    host
}

#[tokio::test]
async fn provider_registration_surfaces_in_the_host() {
    let tmp = tempfile::tempdir().unwrap();
    write_provider_manifest(tmp.path());
    let host = load_host(tmp.path()).await;

    let providers = host.providers();
    assert_eq!(providers.len(), 1, "expected one registered provider");
    let (ext, reg) = &providers[0];
    assert_eq!(ext, "echo");
    assert_eq!(reg.name, "echo-provider");
    assert!(reg.oauth);
    assert_eq!(reg.models.len(), 1);
    assert_eq!(reg.models[0].id, "echo-1");
    assert_eq!(reg.models[0].display_name.as_deref(), Some("Echo One"));

    host.shutdown_all().await;
}

#[tokio::test]
async fn proxied_stream_yields_deltas_then_terminal_done() {
    let tmp = tempfile::tempdir().unwrap();
    write_provider_manifest(tmp.path());
    let host = load_host(tmp.path()).await;

    let provider = host.provider_for("echo-provider", "echo-1", None).expect("provider exists");
    let user = Message::user("hi");
    let stream = provider.chat_stream(&[&user], &[]).await.expect("stream opens");
    let events: Vec<StreamEvent> = stream.collect::<Vec<_>>().await.into_iter().map(|e| e.expect("event")).collect();

    // The two proxied delta chunks arrive IN ORDER before the terminal events.
    let deltas: Vec<String> = events
        .iter()
        .filter_map(|e| match e {
            StreamEvent::Delta { content } => Some(content.clone()),
            _ => None,
        })
        .collect();
    assert_eq!(deltas, vec!["Hel".to_string(), "lo".to_string()], "deltas must stream in order before Done");

    // Terminal Done is last, carrying the finish reason from the final reply.
    assert!(
        matches!(events.last(), Some(StreamEvent::Done { finish_reason }) if finish_reason == "stop"),
        "stream must end with a Done: {events:?}"
    );
    // Usage + resolved model came through the final reply.
    assert!(events.iter().any(|e| matches!(e, StreamEvent::Usage(u) if u.total_tokens == 5)));
    assert!(events.iter().any(|e| matches!(e, StreamEvent::Model { name } if name == "echo-1")));

    host.shutdown_all().await;
}

#[tokio::test]
async fn non_streaming_completion_returns_the_full_response() {
    let tmp = tempfile::tempdir().unwrap();
    write_provider_manifest(tmp.path());
    let host = load_host(tmp.path()).await;

    let provider = host.provider_for("echo-provider", "echo-1", None).expect("provider exists");
    let user = Message::user("hi");
    let resp = provider.chat(&[&user], &[]).await.expect("chat completes");
    assert_eq!(resp.content, "Hello");
    assert_eq!(resp.finish_reason, "stop");
    assert_eq!(resp.usage.total_tokens, 5);
    assert_eq!(resp.resolved_model.as_deref(), Some("echo-1"));

    host.shutdown_all().await;
}

#[tokio::test]
async fn oauth_login_and_refresh_round_trip() {
    let tmp = tempfile::tempdir().unwrap();
    write_provider_manifest(tmp.path());
    let host = load_host(tmp.path()).await;

    let creds = host.provider_oauth_login("echo-provider").await.expect("login");
    assert_eq!(creds.api_key.as_deref(), Some("sk-echo-oauth"));
    assert_eq!(creds.refresh_token.as_deref(), Some("rt-echo"));
    assert_eq!(creds.expires_at, Some(1_900_000_000));

    let refreshed = host.provider_oauth_refresh("echo-provider", "rt-echo").await.expect("refresh");
    assert_eq!(refreshed.api_key.as_deref(), Some("sk-echo-refreshed"));
    assert_eq!(refreshed.refresh_token.as_deref(), Some("rt-echo"), "peer echoes the presented refresh token");

    host.shutdown_all().await;
}

#[tokio::test]
async fn unknown_provider_has_no_proxy_and_oauth_is_method_not_found() {
    let tmp = tempfile::tempdir().unwrap();
    write_provider_manifest(tmp.path());
    let host = load_host(tmp.path()).await;

    assert!(host.provider_for("nope", "m", None).is_none());
    let err = host.provider_oauth_login("nope").await.expect_err("unknown provider errors");
    assert_eq!(err.code, -32601);

    host.shutdown_all().await;
}
