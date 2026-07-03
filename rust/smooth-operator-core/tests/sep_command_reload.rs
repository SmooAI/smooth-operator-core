//! Phase 4 integration: command dispatch + autocomplete round-trips against the
//! reference peer, and hot reload survives (respawn + re-init) while the epoch
//! fence invalidates the pre-reload context. Uses the checked-in `sep-echo-peer`
//! so the suite needs no Node runtime.

use std::sync::Arc;

use smooth_operator_core::extension::protocol::{HostInfo, Tier, WorkspaceInfo};
use smooth_operator_core::extension::{discover, DefaultHostDelegate, ExtensionHost};

/// Write an `echo` extension manifest pointing at the reference peer.
fn write_manifest(global: &std::path::Path) {
    let dir = global.join("echo");
    std::fs::create_dir_all(&dir).unwrap();
    let peer = env!("CARGO_BIN_EXE_sep-echo-peer");
    let toml = format!("name = \"echo\"\nversion = \"0.1.0\"\n[run]\ncommand = \"{peer}\"\n[capabilities]\ntools = true\ncommands = true\n");
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
async fn commands_and_shortcuts_surface_from_registrations() {
    let tmp = tempfile::tempdir().unwrap();
    write_manifest(tmp.path());
    let host = load_host(tmp.path()).await;

    let cmds = host.commands();
    assert_eq!(cmds.len(), 1);
    assert_eq!(cmds[0].0, "echo"); // owning extension
    assert_eq!(cmds[0].1.name, "echo-cmd");

    let shortcuts = host.shortcuts();
    assert_eq!(shortcuts.len(), 1);
    assert_eq!(shortcuts[0].1.key, "ctrl+e");
    assert_eq!(shortcuts[0].1.command, "echo-cmd");
}

#[tokio::test]
async fn run_command_dispatches_and_returns_content() {
    let tmp = tempfile::tempdir().unwrap();
    write_manifest(tmp.path());
    let host = load_host(tmp.path()).await;

    let out = host.run_command(None, "echo-cmd", serde_json::json!({ "x": 1 })).await.expect("run_command");
    let content = out.content.expect("content");
    assert!(content.starts_with("ran echo-cmd"), "got: {content}");
    assert!(content.contains("\"x\":1"), "arguments should round-trip: {content}");

    // An unregistered command is a MethodNotFound (-32601).
    let err = host.run_command(None, "nope", serde_json::json!({})).await.unwrap_err();
    assert_eq!(err.code, -32601);
}

#[tokio::test]
async fn complete_command_round_trips() {
    let tmp = tempfile::tempdir().unwrap();
    write_manifest(tmp.path());
    let host = load_host(tmp.path()).await;

    let completions = host.complete_command(None, "echo-cmd", "foo").await;
    assert_eq!(completions.len(), 1);
    assert_eq!(completions[0].value, "foo-done");

    // Completion for an unknown command is empty, never an error.
    assert!(host.complete_command(None, "nope", "x").await.is_empty());
}

#[tokio::test]
async fn reload_bumps_epoch_and_keeps_the_extension_live() {
    let tmp = tempfile::tempdir().unwrap();
    write_manifest(tmp.path());
    let host = load_host(tmp.path()).await;

    // Context token before reload embeds epoch 1.
    let before = host.context(Tier::Command);
    assert_eq!(before.token, "epoch-1");

    host.reload("echo").await.expect("reload");

    // The epoch fence advanced — every pre-reload token is now stale.
    let after = host.context(Tier::Command);
    assert_eq!(after.token, "epoch-2");
    assert_ne!(before.token, after.token);

    // The extension survived the respawn: its command still dispatches, and its
    // registrations re-loaded (so commands()/tools() still see it).
    assert_eq!(host.len(), 1);
    assert_eq!(host.commands().len(), 1);
    let out = host.run_command(None, "echo-cmd", serde_json::json!({})).await.expect("run after reload");
    assert!(out.content.unwrap().starts_with("ran echo-cmd"));

    // Reloading an unknown extension is an error, not a panic.
    assert!(host.reload("ghost").await.is_err());
}
