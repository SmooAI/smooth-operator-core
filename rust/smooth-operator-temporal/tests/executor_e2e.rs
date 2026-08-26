//! End-to-end test of the client-side [`TemporalExecutor`] — the Rust parity of
//! the TS/Go/Python/.NET `TemporalAgentExecutor`s (ADR-030, th-db0816).
//!
//! The existing e2e files prove the WORKFLOW; this proves the EXECUTOR: a
//! consumer holding only `Arc<dyn AgentExecutor>` drives a durable turn through
//! `execute` and `execute_streaming` with no knowledge of Temporal. Self-skips
//! if the ephemeral dev server can't start (offline/CI), like its siblings.
//!
//! ```sh
//! cargo test -p smooai-smooth-operator-temporal --features temporal --test executor_e2e
//! ```

#![cfg(feature = "temporal")]

use std::sync::Arc;
use std::time::Duration;

use smooai_smooth_operator_temporal::temporal::{
    init_engine, AgentTurnActivities, AgentTurnWorkflow, EngineHandles, TemporalExecutor, TemporalExecutorOptions,
};
use smooth_operator_core::agent::{Agent, AgentConfig, AgentEvent};
use smooth_operator_core::executor::AgentExecutor;
use smooth_operator_core::llm::LlmConfig;
use smooth_operator_core::llm_provider::MockLlmClient;
use smooth_operator_core::tool::ToolRegistry;
use temporalio_client::{Client, ClientOptions, Connection, ConnectionOptions};
use temporalio_common::telemetry::TelemetryOptions;
use temporalio_sdk::{Worker, WorkerOptions};
use temporalio_sdk_core::ephemeral_server::{default_cached_download, TemporalDevServerConfig};
use temporalio_sdk_core::{CoreRuntime, RuntimeOptions};
use url::Url;

const TASK_QUEUE: &str = "smooth-operator-temporal-executor-test";

#[tokio::test]
async fn executor_runs_durable_turns_behind_the_agent_executor_trait() -> anyhow::Result<()> {
    // Two scripted replies: one for `execute`, one for `execute_streaming`.
    let mock = MockLlmClient::new();
    mock.push_text("durable via execute");
    mock.push_text("durable via streaming");
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
            .identity("smooth-operator-temporal-executor-test".to_owned())
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

    // The consumer's view: a dyn AgentExecutor — no Temporal types in sight.
    let executor: Arc<dyn AgentExecutor> = Arc::new(TemporalExecutor::new(
        client.clone(),
        TemporalExecutorOptions {
            task_queue: TASK_QUEUE.to_string(),
            system_prompt: "You are a durable test agent".to_string(),
            max_iterations: 5,
            ..Default::default()
        },
    ));
    // The agent supplies only its id (its config lives, conceptually, in the
    // worker process — the engine-handle split shared with all four ports).
    let agent = Agent::new(
        AgentConfig::new(
            "durable-agent",
            "unused — the executor's options govern the turn",
            LlmConfig::openrouter("fake-key"),
        ),
        ToolRegistry::new(),
    );
    let agent_uuid = agent.id.clone();

    let client_work = async move {
        // 1. `execute`: the trait's plain entry point.
        let convo = executor.execute(&agent, "first durable question".to_string()).await?;

        // 2. `execute_streaming`: same durable turn; only a terminal Completed
        //    event (a workflow has no token-delta channel — ADR-030 gap shared
        //    with the four ports).
        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let convo_streaming = executor.execute_streaming(&agent, "second durable question".to_string(), tx).await?;
        let mut events = Vec::new();
        while let Ok(ev) = rx.try_recv() {
            events.push(ev);
        }
        shutdown();
        anyhow::Ok((convo, convo_streaming, events))
    };

    let (worker_res, client_res) = tokio::join!(worker.run(), client_work);
    worker_res?;
    let (convo, convo_streaming, events) = client_res?;

    // Both turns ran through the workflow with the executor's configuration.
    assert_eq!(convo.last_assistant_content(), Some("durable via execute"));
    assert_eq!(convo_streaming.last_assistant_content(), Some("durable via streaming"));
    assert_eq!(mock.call_count(), 2);
    let calls = mock.calls();
    assert!(calls[0].messages.iter().any(|m| m.content.contains("first durable question")));
    assert!(calls[0].messages.iter().any(|m| m.content.contains("You are a durable test agent")));

    // The streaming path emitted exactly the terminal Completed, carrying the
    // agent's id and honest estimated-usage flags.
    assert_eq!(events.len(), 1, "expected one terminal event, got: {events:?}");
    match &events[0] {
        AgentEvent::Completed {
            agent_id,
            cost_estimated,
            usage_estimated,
            ..
        } => {
            assert_eq!(agent_id, &agent_uuid);
            assert!(*cost_estimated && *usage_estimated, "durable-turn usage is not measured yet and must say so");
        }
        other => panic!("expected Completed, got: {other:?}"),
    }

    let mut server = server;
    server.shutdown().await.ok();
    Ok(())
}
