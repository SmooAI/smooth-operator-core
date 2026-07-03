//! End-to-end: an extension's `tool_call` hook vetoes a tool BEFORE the
//! registry runs it, and the agent loop is byte-for-byte unchanged when no
//! extension host is attached. This is the "extension mutate → ToolHook veto"
//! layering the plan calls out.

use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use smooth_operator_core::extension::protocol::{HostInfo, WorkspaceInfo};
use smooth_operator_core::extension::{discover, DefaultHostDelegate, ExtensionHost};
use smooth_operator_core::llm_provider::MockLlmClient;
use smooth_operator_core::{Agent, AgentConfig, AgentEvent, LlmConfig, Tool, ToolRegistry, ToolSchema};

/// A native tool that records whether it actually executed.
struct DangerTool {
    ran: Arc<AtomicBool>,
}

#[async_trait]
impl Tool for DangerTool {
    fn schema(&self) -> ToolSchema {
        ToolSchema {
            name: "danger".into(),
            description: "Deletes everything.".into(),
            parameters: serde_json::json!({"type": "object"}),
        }
    }
    async fn execute(&self, _arguments: serde_json::Value) -> anyhow::Result<String> {
        self.ran.store(true, Ordering::SeqCst);
        Ok("did the dangerous thing".into())
    }
}

/// Write an `echo` extension manifest into a temp global dir pointing at the
/// reference peer, optionally in block mode.
fn write_manifest(global: &std::path::Path, block: bool) {
    let env = if block { &[("SEP_ECHO_BLOCK", "1")][..] } else { &[][..] };
    write_manifest_ext(global, env, None);
}

/// Full manifest writer: arbitrary peer env vars + an optional short
/// `hook_timeout_ms` (so the fail-closed timeout test doesn't wait 60s).
fn write_manifest_ext(global: &std::path::Path, env: &[(&str, &str)], hook_timeout_ms: Option<u64>) {
    let dir = global.join("echo");
    std::fs::create_dir_all(&dir).unwrap();
    let peer = env!("CARGO_BIN_EXE_sep-echo-peer");
    let env_line = if env.is_empty() {
        String::new()
    } else {
        let pairs = env.iter().map(|(k, v)| format!("{k} = \"{v}\"")).collect::<Vec<_>>().join(", ");
        format!("env = {{ {pairs} }}\n")
    };
    let timeout_line = hook_timeout_ms.map(|ms| format!("hook_timeout_ms = {ms}\n")).unwrap_or_default();
    let toml = format!("name = \"echo\"\nversion = \"0.1.0\"\n{timeout_line}[run]\ncommand = \"{peer}\"\n{env_line}[capabilities]\ntools = true\n");
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

fn agent_with(ran: Arc<AtomicBool>, events: Arc<Mutex<Vec<AgentEvent>>>, host: Option<Arc<ExtensionHost>>) -> Agent {
    let mock = MockLlmClient::new();
    mock.push_tool_call("c1", "danger", serde_json::json!({}));
    mock.push_text("done");

    let mut registry = ToolRegistry::new();
    registry.register(DangerTool { ran });

    let config = AgentConfig::new("t", "system", LlmConfig::openrouter("fake-key"));
    let ev = Arc::clone(&events);
    let mut agent = Agent::new(config, registry)
        .with_llm_provider(Arc::new(mock))
        .with_event_handler(move |e| ev.lock().unwrap().push(e));
    if let Some(host) = host {
        agent = agent.with_extension_host(host);
    }
    agent
}

#[tokio::test]
async fn extension_vetoes_tool_call_before_registry_runs_it() {
    let tmp = tempfile::tempdir().unwrap();
    write_manifest(tmp.path(), true);
    let host = Arc::new(load_host(tmp.path()).await);

    let ran = Arc::new(AtomicBool::new(false));
    let events = Arc::new(Mutex::new(Vec::new()));
    let agent = agent_with(Arc::clone(&ran), Arc::clone(&events), Some(host));

    let convo = agent.run("go").await.expect("run");

    // The dangerous tool was vetoed — it never executed.
    assert!(!ran.load(Ordering::SeqCst), "danger tool ran despite the extension veto");

    // The veto surfaced as an error tool-result.
    let evs = events.lock().unwrap();
    let blocked = evs
        .iter()
        .any(|e| matches!(e, AgentEvent::ToolCallComplete { is_error, result, .. } if *is_error && result.contains("blocked by extension")));
    assert!(blocked, "expected a blocked ToolCallComplete, got: {evs:?}");

    // The turn still finished cleanly.
    assert_eq!(convo.last_assistant_content(), Some("done"));
}

#[tokio::test]
async fn no_extension_host_is_zero_behavior_change() {
    // Same script, no host: the tool runs exactly as it always has.
    let ran = Arc::new(AtomicBool::new(false));
    let events = Arc::new(Mutex::new(Vec::new()));
    let agent = agent_with(Arc::clone(&ran), Arc::clone(&events), None);

    let convo = agent.run("go").await.expect("run");

    assert!(ran.load(Ordering::SeqCst), "danger tool should run with no extension host");
    let evs = events.lock().unwrap();
    // No SEP turn events are emitted without a host.
    assert!(!evs.iter().any(|e| matches!(e, AgentEvent::TurnStart { .. } | AgentEvent::TurnEnd { .. })));
    assert_eq!(convo.last_assistant_content(), Some("done"));
}

/// The Phase 1 headline: a scripted LLM calls an extension-registered tool
/// (`echo.say`) through the real ExtensionHost, and the extension's reply flows
/// back as the tool result. This is the full schema-on-wire → execute
/// round-trip the plan requires ("LLM calls hello.greet through a real turn").
#[tokio::test]
async fn llm_calls_extension_registered_tool_end_to_end() {
    let tmp = tempfile::tempdir().unwrap();
    write_manifest(tmp.path(), false); // continue every hook
    let host = Arc::new(load_host(tmp.path()).await);

    // The extension registered `say`; the host exposes it as `echo.say`.
    let tool_names: Vec<_> = host.tools().iter().map(|t| t.schema().name).collect();
    assert!(tool_names.contains(&"echo.say".to_string()), "expected echo.say tool, got {tool_names:?}");

    let mock = MockLlmClient::new();
    mock.push_tool_call("c1", "echo.say", serde_json::json!({ "phrase": "hello from the LLM" }));
    mock.push_text("done");

    let events = Arc::new(Mutex::new(Vec::new()));
    let ev = Arc::clone(&events);
    let config = AgentConfig::new("t", "system", LlmConfig::openrouter("fake-key"));
    let agent = Agent::new(config, ToolRegistry::new())
        .with_llm_provider(Arc::new(mock))
        .with_event_handler(move |e| ev.lock().unwrap().push(e))
        .with_extension_host(host);

    let convo = agent.run("go").await.expect("run");

    // The extension tool executed and echoed the phrase back as the result.
    let evs = events.lock().unwrap();
    let echoed = evs.iter().any(|e| {
        matches!(e, AgentEvent::ToolCallComplete { is_error, result, tool_name, .. }
            if !is_error && tool_name == "echo.say" && result.contains("hello from the LLM"))
    });
    assert!(echoed, "expected echo.say to return the phrase, got: {evs:?}");
    assert_eq!(convo.last_assistant_content(), Some("done"));
}

/// Extension tools are ordinary registry tools, so the same `retain` filter a
/// server uses to enforce a per-agent `enabled_tools` allow-list drops them by
/// their dotted name exactly as it drops native tools. (The server never sees
/// an ExtensionHost until a later phase; this proves the composition holds at
/// the registry seam it will use.)
#[tokio::test]
async fn extension_tools_are_filtered_by_registry_retain() {
    let tmp = tempfile::tempdir().unwrap();
    write_manifest(tmp.path(), false);
    let host = load_host(tmp.path()).await;

    let mut registry = ToolRegistry::new();
    registry.register(DangerTool {
        ran: Arc::new(AtomicBool::new(false)),
    });
    for tool in host.tools() {
        registry.register_arc(tool);
    }

    let before: Vec<_> = registry.schemas().into_iter().map(|s| s.name).collect();
    assert!(before.contains(&"danger".to_string()));
    assert!(
        before.contains(&"echo.say".to_string()),
        "ext tool should be visible before filtering: {before:?}"
    );

    // Enforce an allow-list that excludes the extension tool.
    registry.retain(|name| name != "echo.say");

    let after: Vec<_> = registry.schemas().into_iter().map(|s| s.name).collect();
    assert!(after.contains(&"danger".to_string()), "native tool survives the allow-list");
    assert!(!after.contains(&"echo.say".to_string()), "ext tool filtered out exactly like a native tool");
}

/// The Phase 2 headline: a `tool_call` hook that HANGS must not stall the turn.
/// The host times out (short `hook_timeout_ms`), and because `tool_call` is
/// fail-closed, the tool is BLOCKED — the turn still completes promptly.
#[tokio::test]
async fn hung_tool_call_hook_fails_closed_without_stalling() {
    let tmp = tempfile::tempdir().unwrap();
    // Peer hangs on every hook; a 200ms hook timeout keeps the test fast.
    write_manifest_ext(tmp.path(), &[("SEP_ECHO_HANG", "1")], Some(200));
    let host = Arc::new(load_host(tmp.path()).await);

    let ran = Arc::new(AtomicBool::new(false));
    let events = Arc::new(Mutex::new(Vec::new()));
    let agent = agent_with(Arc::clone(&ran), Arc::clone(&events), Some(host));

    // Bound the whole run: if the hang stalled the turn, this times out and fails.
    let convo = tokio::time::timeout(std::time::Duration::from_secs(10), agent.run("go"))
        .await
        .expect("agent run stalled — fail-closed hook did not unblock the turn")
        .expect("run");

    // Fail-closed: the dangerous tool never executed.
    assert!(!ran.load(Ordering::SeqCst), "danger tool ran despite the hung (fail-closed) hook");

    // The veto surfaced as a fail-closed error tool-result.
    let evs = events.lock().unwrap();
    let blocked = evs.iter().any(|e| {
        matches!(e, AgentEvent::ToolCallComplete { is_error, result, .. }
            if *is_error && result.contains("fail-closed"))
    });
    assert!(blocked, "expected a fail-closed ToolCallComplete, got: {evs:?}");
    assert_eq!(convo.last_assistant_content(), Some("done"));
}

/// The `tool_result` hook (fail-open) patches a completed tool result BEFORE it
/// is pushed to the conversation. The tool still runs; its output is rewritten.
#[tokio::test]
async fn tool_result_hook_patches_result_content() {
    let tmp = tempfile::tempdir().unwrap();
    write_manifest_ext(tmp.path(), &[("SEP_ECHO_PATCH", "1")], None);
    let host = Arc::new(load_host(tmp.path()).await);

    let ran = Arc::new(AtomicBool::new(false));
    let events = Arc::new(Mutex::new(Vec::new()));
    let agent = agent_with(Arc::clone(&ran), Arc::clone(&events), Some(host));

    let convo = agent.run("go").await.expect("run");

    // tool_call continued → the tool executed.
    assert!(ran.load(Ordering::SeqCst), "tool should run (tool_call continued)");
    // tool_result rewrote the content the agent saw.
    let evs = events.lock().unwrap();
    let patched = evs
        .iter()
        .any(|e| matches!(e, AgentEvent::ToolCallComplete { result, .. } if result.contains("[patched by echo]")));
    assert!(patched, "expected the tool_result hook to patch the content, got: {evs:?}");
    assert_eq!(convo.last_assistant_content(), Some("done"));
}

#[tokio::test]
async fn non_blocking_extension_lets_tool_run_and_emits_turn_events() {
    let tmp = tempfile::tempdir().unwrap();
    write_manifest(tmp.path(), false); // echo peer continues every hook
    let host = Arc::new(load_host(tmp.path()).await);

    let ran = Arc::new(AtomicBool::new(false));
    let events = Arc::new(Mutex::new(Vec::new()));
    let agent = agent_with(Arc::clone(&ran), Arc::clone(&events), Some(host));

    let convo = agent.run("go").await.expect("run");

    assert!(ran.load(Ordering::SeqCst), "danger tool should run when the extension continues");
    let evs = events.lock().unwrap();
    assert!(
        evs.iter().any(|e| matches!(e, AgentEvent::TurnStart { .. })),
        "expected TurnStart with a host attached"
    );
    assert!(
        evs.iter().any(|e| matches!(e, AgentEvent::MessageEnd { .. })),
        "expected MessageEnd with a host attached"
    );
    assert_eq!(convo.last_assistant_content(), Some("done"));
}
