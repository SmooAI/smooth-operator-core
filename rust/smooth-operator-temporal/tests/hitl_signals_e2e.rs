//! End-to-end test of **durable human-in-the-loop** via Temporal signals.
//!
//! A turn whose model calls an approval-gated tool blocks the workflow until an
//! `approve_tool` / `deny_tool` signal names that tool call. We run two turns
//! against an ephemeral dev server: one **approved** (the tool runs), one
//! **denied** (the tool is skipped with an error result the model sees). This is
//! the durable HITL unlock — the block is recorded in workflow history, so it
//! survives restarts and can resolve arbitrarily later.
//!
//! Self-skips if the ephemeral server can't be started (offline/CI). Run with:
//! `cargo test -p smooai-smooth-operator-temporal --features temporal`.

#![cfg(feature = "temporal")]

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use smooai_smooth_operator_temporal::temporal::{init_engine, AgentTurnActivities, AgentTurnInput, AgentTurnWorkflow, EngineHandles};
use smooth_operator_core::conversation::{Conversation, Role};
use smooth_operator_core::llm_provider::MockLlmClient;
use smooth_operator_core::tool::{Tool, ToolRegistry, ToolSchema};
use temporalio_client::{Client, ClientOptions, Connection, ConnectionOptions, WorkflowGetResultOptions, WorkflowSignalOptions, WorkflowStartOptions};
use temporalio_common::telemetry::TelemetryOptions;
use temporalio_sdk::{Worker, WorkerOptions};
use temporalio_sdk_core::ephemeral_server::{default_cached_download, TemporalDevServerConfig};
use temporalio_sdk_core::{CoreRuntime, RuntimeOptions};
use url::Url;

const TASK_QUEUE: &str = "smooth-operator-temporal-hitl-test";

struct EchoTool;

#[async_trait]
impl Tool for EchoTool {
    fn schema(&self) -> ToolSchema {
        ToolSchema {
            name: "echo".into(),
            description: "Echoes input back".into(),
            parameters: serde_json::json!({ "type": "object", "properties": { "text": {"type": "string"} }, "required": ["text"] }),
        }
    }

    async fn execute(&self, arguments: serde_json::Value) -> anyhow::Result<String> {
        Ok(arguments["text"].as_str().unwrap_or("").to_string())
    }
}

#[tokio::test]
async fn hitl_gate_approves_and_denies_via_signals() -> anyhow::Result<()> {
    // One mock model, scripted FIFO across BOTH (sequential) turns:
    //   turn 1 (approved): echo tool call, then a wrap-up reply
    //   turn 2 (denied):   echo tool call, then a wrap-up reply
    let mock = MockLlmClient::new();
    mock.push_tool_call("call-approve", "echo", serde_json::json!({ "text": "ran-after-approval" }));
    mock.push_text("done after approval");
    mock.push_tool_call("call-deny", "echo", serde_json::json!({ "text": "should-not-run" }));
    mock.push_text("done after denial");

    let mut registry = ToolRegistry::new();
    registry.register(EchoTool);
    if init_engine(EngineHandles {
        llm: Arc::new(mock.clone()),
        tools: Arc::new(registry),
    })
    .is_err()
    {
        eprintln!("SKIP: engine handles already initialized");
        return Ok(());
    }

    let server = match tokio::time::timeout(
        Duration::from_secs(120),
        TemporalDevServerConfig::builder().exe(default_cached_download()).build().start_server(),
    )
    .await
    {
        Ok(Ok(s)) => s,
        Ok(Err(e)) => {
            eprintln!("SKIP: could not start ephemeral Temporal dev server (likely offline): {e}");
            return Ok(());
        }
        Err(_) => {
            eprintln!("SKIP: ephemeral Temporal dev server did not start within 120s");
            return Ok(());
        }
    };

    let runtime_options = RuntimeOptions::builder()
        .telemetry_options(TelemetryOptions::builder().build())
        .build()
        .map_err(|e| anyhow::anyhow!("runtime options: {e}"))?;
    let runtime = CoreRuntime::new_assume_tokio(runtime_options).map_err(|e| anyhow::anyhow!("core runtime: {e}"))?;

    let target = Url::parse(&format!("http://{}", server.target))?;
    let connection = Connection::connect(ConnectionOptions::new(target).identity("smooth-operator-temporal-hitl-test".to_owned()).build()).await?;
    let client = Client::new(connection, ClientOptions::new("default").build()).map_err(|e| anyhow::anyhow!("client: {e}"))?;

    let worker_options = WorkerOptions::new(TASK_QUEUE)
        .register_workflow::<AgentTurnWorkflow>()
        .register_activities(AgentTurnActivities)
        .build();
    let mut worker = Worker::new(&runtime, client.clone(), worker_options).map_err(|e| anyhow::anyhow!("worker: {e}"))?;
    let shutdown = worker.shutdown_handle();

    let starter = client.clone();
    let client_work = async move {
        let input = |id: &str| AgentTurnInput {
            system_prompt: "You are a gated agent".to_string(),
            user_message: format!("use echo ({id})"),
            tools: vec![],
            max_iterations: 5,
            approval_required_tools: vec!["echo".to_string()],
        };

        // --- Turn 1: APPROVE ---
        let approve_handle = starter
            .start_workflow(
                AgentTurnWorkflow::run,
                input("approve"),
                WorkflowStartOptions::new(TASK_QUEUE, "hitl-approve").build(),
            )
            .await?;
        // The tool-call id is deterministic from the mock script; the signal
        // buffers, so the gate sees it approved when it checks.
        approve_handle
            .signal(AgentTurnWorkflow::approve_tool, "call-approve".to_string(), WorkflowSignalOptions::default())
            .await?;
        let approved: Conversation = approve_handle.get_result(WorkflowGetResultOptions::default()).await?;

        // --- Turn 2: DENY ---
        let deny_handle = starter
            .start_workflow(
                AgentTurnWorkflow::run,
                input("deny"),
                WorkflowStartOptions::new(TASK_QUEUE, "hitl-deny").build(),
            )
            .await?;
        deny_handle
            .signal(AgentTurnWorkflow::deny_tool, "call-deny".to_string(), WorkflowSignalOptions::default())
            .await?;
        let denied: Conversation = deny_handle.get_result(WorkflowGetResultOptions::default()).await?;

        shutdown();
        anyhow::Ok((approved, denied))
    };

    let (worker_res, client_res): (Result<(), anyhow::Error>, anyhow::Result<(Conversation, Conversation)>) = tokio::join!(worker.run(), client_work);
    worker_res?;
    let (approved, denied) = client_res?;

    // Approved turn: the gated tool actually ran, its real result is in the
    // conversation, and the turn finished.
    let approved_tool: Vec<&_> = approved.messages.iter().filter(|m| m.role == Role::Tool).collect();
    assert_eq!(approved_tool.len(), 1);
    assert_eq!(approved_tool[0].content, "ran-after-approval");
    assert_eq!(approved.last_assistant_content(), Some("done after approval"));

    // Denied turn: the tool NEVER ran — the tool message is a denial error, not
    // the echo payload — and the model still got to wrap up.
    let denied_tool: Vec<&_> = denied.messages.iter().filter(|m| m.role == Role::Tool).collect();
    assert_eq!(denied_tool.len(), 1);
    assert!(denied_tool[0].content.contains("denied by human approval"));
    assert_ne!(denied_tool[0].content, "should-not-run");
    assert_eq!(denied.last_assistant_content(), Some("done after denial"));

    let mut server = server;
    server.shutdown().await.ok();
    Ok(())
}
