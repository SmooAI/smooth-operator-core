//! Phase 8 wire paths: the inter-extension bus (`bus/publish` → `bus/event`
//! fanout) and declarative message renderers. Both drive the reference
//! `sep-echo-peer` as real subprocesses through the engine host.

use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use async_trait::async_trait;
use serde_json::{json, Value};
use smooth_operator_core::extension::protocol::{HostInfo, RpcError, WorkspaceInfo};
use smooth_operator_core::extension::{discover, ExtensionHost, HostDelegate};

/// Writes an echo-peer manifest named `name` into `global/<name>/`, wiring the
/// given peer env vars.
fn write_peer(global: &std::path::Path, name: &str, env: &[(&str, &str)]) {
    let dir = global.join(name);
    std::fs::create_dir_all(&dir).unwrap();
    let peer = env!("CARGO_BIN_EXE_sep-echo-peer");
    let env_line = if env.is_empty() {
        String::new()
    } else {
        let pairs = env.iter().map(|(k, v)| format!("{k} = \"{v}\"")).collect::<Vec<_>>().join(", ");
        format!("env = {{ {pairs} }}\n")
    };
    // The peer always names itself "echo" at handshake; the manifest name is the
    // discovery/registry key. Namespacing (`<name>.say`) uses the manifest name.
    let toml = format!("name = \"{name}\"\nversion = \"0.1.0\"\n[run]\ncommand = \"{peer}\"\n{env_line}[capabilities]\ntools = true\n");
    std::fs::write(dir.join("extension.toml"), toml).unwrap();
}

async fn load(global: &std::path::Path, delegate: Arc<dyn HostDelegate>) -> ExtensionHost {
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
        delegate,
    )
    .await;
    assert!(load_failures.is_empty(), "load failures: {load_failures:?}");
    host
}

/// Records `kv/set` calls so a test can observe a subscriber reacting to a
/// bus/event fanout.
#[derive(Default)]
struct KvRecordingDelegate {
    seen: Mutex<Vec<(String, Value)>>,
}

#[async_trait]
impl HostDelegate for KvRecordingDelegate {
    async fn kv_set(&self, _ext: &str, key: &str, value: Value) -> Result<(), RpcError> {
        self.seen.lock().unwrap().push((key.to_string(), value));
        Ok(())
    }
}

#[tokio::test]
async fn bus_publish_fans_out_to_subscribers() {
    let tmp = tempfile::tempdir().unwrap();
    write_peer(tmp.path(), "pub", &[("SEP_ECHO_BUS_PUB", "1")]);
    write_peer(tmp.path(), "sub", &[("SEP_ECHO_BUS_SUB", "1")]);

    let delegate = Arc::new(KvRecordingDelegate::default());
    let host = load(tmp.path(), Arc::clone(&delegate) as Arc<dyn HostDelegate>).await;
    assert_eq!(host.len(), 2, "both extensions should load");

    // Drive the publisher's tool — it emits a `bus/publish` before replying. The
    // host fans it to `sub` (subscribed to bus/event), which records the topic
    // via kv/set. No agent/LLM needed: we call the tool proxy directly.
    let pub_tool = host.tools().into_iter().find(|t| t.schema().name == "pub.say").expect("pub.say tool");
    let out = pub_tool.execute(json!({ "phrase": "go" })).await.expect("pub tool ran");
    assert_eq!(out, "published");

    // The fanout is fire-and-forget across two subprocesses, so poll briefly.
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if delegate.seen.lock().unwrap().iter().any(|(k, v)| k == "bus_seen" && v == &json!("ping")) {
            break;
        }
        assert!(
            Instant::now() < deadline,
            "sub never received the bus/event (kv sets: {:?})",
            delegate.seen.lock().unwrap()
        );
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
}

#[tokio::test]
async fn widget_key_is_routed_to_the_named_extension() {
    let tmp = tempfile::tempdir().unwrap();
    // No SEP_ECHO_BUS_SUB: the peer does NOT subscribe to widget/key, proving
    // the targeted dispatch bypasses the observe subscription filter.
    write_peer(tmp.path(), "w", &[]);

    let delegate = Arc::new(KvRecordingDelegate::default());
    let host = load(tmp.path(), Arc::clone(&delegate) as Arc<dyn HostDelegate>).await;
    host.dispatch_widget_key("w", json!({ "key": "ArrowUp" }));

    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if delegate.seen.lock().unwrap().iter().any(|(k, v)| k == "widget_key" && v == &json!("ArrowUp")) {
            break;
        }
        assert!(
            Instant::now() < deadline,
            "widget owner never received the key (kv sets: {:?})",
            delegate.seen.lock().unwrap()
        );
        tokio::time::sleep(Duration::from_millis(25)).await;
    }

    // An unknown target is a silent no-op (must not panic).
    host.dispatch_widget_key("nonexistent", json!({ "key": "x" }));
}

#[tokio::test]
async fn message_renderers_are_surfaced() {
    let tmp = tempfile::tempdir().unwrap();
    write_peer(tmp.path(), "rend", &[("SEP_ECHO_RENDERER", "1")]);

    let host = load(tmp.path(), Arc::new(smooth_operator_core::extension::DefaultHostDelegate)).await;
    let renderers = host.message_renderers();
    assert_eq!(renderers.len(), 1, "expected one message renderer, got {renderers:?}");
    assert_eq!(renderers[0].tag, "echo_card");
    assert_eq!(renderers[0].template.get("kind").and_then(Value::as_str), Some("markdown"));
}
