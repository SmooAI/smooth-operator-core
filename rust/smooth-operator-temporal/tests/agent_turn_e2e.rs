//! End-to-end test of a **real agent turn** running through `AgentTurnWorkflow`
//! against an ephemeral Temporal dev server.
//!
//! The model call is backed by a `MockLlmClient` installed via `init_engine`, so
//! the whole per-step path is exercised: workflow → `model_call` activity (mock
//! model) → engine `drive_turn` → returned `Conversation`. This is the proof that
//! the durable backend runs the same loop as the in-process path.
//!
//! Self-skips if the ephemeral server can't be downloaded/started (offline/CI),
//! mirroring the engine's Docker-gated Postgres tests. Run with:
//!
//! ```sh
//! cargo test -p smooai-smooth-operator-temporal --features temporal
//! ```

#![cfg(feature = "temporal")]

use std::sync::Arc;
use std::time::Duration;

use smooai_smooth_operator_temporal::temporal::{init_engine, AgentTurnActivities, AgentTurnInput, AgentTurnWorkflow, EngineHandles, HealthWorkflow};
use smooth_operator_core::conversation::Conversation;
use smooth_operator_core::llm_provider::MockLlmClient;
use smooth_operator_core::tool::ToolRegistry;
use temporalio_client::{Client, ClientOptions, Connection, ConnectionOptions, WorkflowGetResultOptions, WorkflowStartOptions};
use temporalio_common::telemetry::TelemetryOptions;
use temporalio_sdk::{Worker, WorkerOptions};
use temporalio_sdk_core::ephemeral_server::{default_cached_download, TemporalDevServerConfig};
use temporalio_sdk_core::{CoreRuntime, RuntimeOptions};
use url::Url;

const TASK_QUEUE: &str = "smooth-operator-temporal-agent-turn-test";

#[tokio::test]
async fn agent_turn_workflow_runs_a_real_turn_end_to_end() -> anyhow::Result<()> {
    // Install a mock model so the `model_call` activity returns a scripted reply.
    // (Set once per test process; this is the only test in this binary.)
    let mock = MockLlmClient::new();
    mock.push_text("the durable answer is 42");
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
            .identity("smooth-operator-temporal-agent-turn-test".to_owned())
            .build(),
    )
    .await?;
    let client = Client::new(connection, ClientOptions::new("default").build()).map_err(|e| anyhow::anyhow!("client: {e}"))?;

    // Register both workflows + the activities (which read the mock via the engine global).
    let worker_options = WorkerOptions::new(TASK_QUEUE)
        .register_workflow::<AgentTurnWorkflow>()
        .register_workflow::<HealthWorkflow>()
        .register_activities(AgentTurnActivities)
        .build();
    let mut worker = Worker::new(&runtime, client.clone(), worker_options).map_err(|e| anyhow::anyhow!("worker: {e}"))?;
    let shutdown = worker.shutdown_handle();

    let starter = client.clone();
    let client_work = async move {
        let input = AgentTurnInput {
            system_prompt: "You are a test agent".to_string(),
            user_message: "what is the durable answer?".to_string(),
            tools: vec![],
            max_iterations: 5,
            approval_required_tools: vec![],
        };
        let handle = starter
            .start_workflow(AgentTurnWorkflow::run, input, WorkflowStartOptions::new(TASK_QUEUE, "agent-turn-e2e-1").build())
            .await?;
        let conversation: Conversation = handle.get_result(WorkflowGetResultOptions::default()).await?;
        shutdown();
        anyhow::Ok(conversation)
    };

    let (worker_res, client_res): (Result<(), anyhow::Error>, anyhow::Result<Conversation>) = tokio::join!(worker.run(), client_work);
    worker_res?;
    let conversation = client_res?;

    // The turn ran through the workflow: the mock's scripted reply is the final
    // assistant message, and the model was called exactly once (no tools).
    assert_eq!(conversation.last_assistant_content(), Some("the durable answer is 42"));
    assert_eq!(mock.call_count(), 1);
    // The user's message reached the model through the activity boundary.
    let calls = mock.calls();
    assert!(calls[0].messages.iter().any(|m| m.content.contains("what is the durable answer?")));

    let mut server = server;
    server.shutdown().await.ok();
    Ok(())
}
