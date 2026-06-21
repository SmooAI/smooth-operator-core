//! End-to-end test of a **durable timer** — an agent that pauses itself on a
//! Temporal timer mid-turn, then resumes.
//!
//! The model calls the configured `wait` tool; the workflow sleeps on
//! `ctx.timer` (recorded in history, so it survives restarts and can span days)
//! and then continues the turn. We use a short (1s) real timer against the dev
//! server, which reliably proves the durable pause without depending on
//! time-skipping mechanics.
//!
//! Self-skips if the ephemeral server can't be started (offline/CI). Run with:
//! `cargo test -p smooai-smooth-operator-temporal --features temporal`.

#![cfg(feature = "temporal")]

use std::sync::Arc;
use std::time::{Duration, Instant};

use smooai_smooth_operator_temporal::temporal::{init_engine, AgentTurnActivities, AgentTurnInput, AgentTurnWorkflow, EngineHandles};
use smooth_operator_core::conversation::{Conversation, Role};
use smooth_operator_core::llm_provider::MockLlmClient;
use smooth_operator_core::tool::ToolRegistry;
use temporalio_client::{Client, ClientOptions, Connection, ConnectionOptions, WorkflowGetResultOptions, WorkflowStartOptions};
use temporalio_common::telemetry::TelemetryOptions;
use temporalio_sdk::{Worker, WorkerOptions};
use temporalio_sdk_core::ephemeral_server::{default_cached_download, TemporalDevServerConfig};
use temporalio_sdk_core::{CoreRuntime, RuntimeOptions};
use url::Url;

const TASK_QUEUE: &str = "smooth-operator-temporal-timer-test";

#[tokio::test]
async fn durable_wait_tool_sleeps_on_a_timer_then_resumes() -> anyhow::Result<()> {
    // The model asks to wait 1 second, then wraps up.
    let mock = MockLlmClient::new();
    mock.push_tool_call("call-wait", "wait", serde_json::json!({ "seconds": 1 }));
    mock.push_text("resumed after the timer");

    // No tools registered — the `wait` tool is handled by the workflow, not the
    // tool registry/activity.
    if init_engine(EngineHandles {
        llm: Arc::new(mock.clone()),
        tools: Arc::new(ToolRegistry::new()),
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
    let connection = Connection::connect(
        ConnectionOptions::new(target)
            .identity("smooth-operator-temporal-timer-test".to_owned())
            .build(),
    )
    .await?;
    let client = Client::new(connection, ClientOptions::new("default").build()).map_err(|e| anyhow::anyhow!("client: {e}"))?;

    let worker_options = WorkerOptions::new(TASK_QUEUE)
        .register_workflow::<AgentTurnWorkflow>()
        .register_activities(AgentTurnActivities)
        .build();
    let mut worker = Worker::new(&runtime, client.clone(), worker_options).map_err(|e| anyhow::anyhow!("worker: {e}"))?;
    let shutdown = worker.shutdown_handle();

    let started = Instant::now();
    let starter = client.clone();
    let client_work = async move {
        let input = AgentTurnInput {
            system_prompt: "You are a self-pacing agent".to_string(),
            user_message: "wait a moment, then answer".to_string(),
            tools: vec![],
            max_iterations: 5,
            approval_required_tools: vec![],
            wait_tool: Some("wait".to_string()),
        };
        let handle = starter
            .start_workflow(AgentTurnWorkflow::run, input, WorkflowStartOptions::new(TASK_QUEUE, "durable-timer-1").build())
            .await?;
        let conversation: Conversation = handle.get_result(WorkflowGetResultOptions::default()).await?;
        shutdown();
        anyhow::Ok(conversation)
    };

    let (worker_res, client_res): (Result<(), anyhow::Error>, anyhow::Result<Conversation>) = tokio::join!(worker.run(), client_work);
    worker_res?;
    let conversation = client_res?;
    let elapsed = started.elapsed();

    // The wait tool produced a durable-timer result, and the turn resumed after.
    let tool_msgs: Vec<&_> = conversation.messages.iter().filter(|m| m.role == Role::Tool).collect();
    assert_eq!(tool_msgs.len(), 1);
    assert!(
        tool_msgs[0].content.contains("durable timer"),
        "unexpected wait result: {}",
        tool_msgs[0].content
    );
    assert_eq!(conversation.last_assistant_content(), Some("resumed after the timer"));
    // It actually waited (the 1s timer elapsed), not skipped instantly.
    assert!(
        elapsed >= Duration::from_millis(900),
        "turn returned too fast to have honored the timer: {elapsed:?}"
    );

    let mut server = server;
    server.shutdown().await.ok();
    Ok(())
}
