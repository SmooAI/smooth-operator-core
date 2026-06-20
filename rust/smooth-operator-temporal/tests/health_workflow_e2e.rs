//! End-to-end integration test against a real, ephemeral Temporal dev server.
//!
//! The SDK's `ephemeral_server` auto-downloads (and caches) the Temporal CLI
//! binary and starts a local dev server in-process — **no Docker, no manually
//! installed `temporal` CLI**. We stand up a worker, execute the scaffold
//! `HealthWorkflow` end to end through it, and assert the activity result came
//! back through the workflow.
//!
//! Self-skipping: if the server can't be downloaded/started (e.g. no network in
//! CI), the test logs a skip and passes rather than failing — mirroring the
//! engine's Docker-gated Postgres tests. Run it explicitly with:
//!
//! ```sh
//! cargo test -p smooai-smooth-operator-temporal --features temporal
//! ```

#![cfg(feature = "temporal")]

use std::time::Duration;

use smooai_smooth_operator_temporal::temporal::{AgentTurnActivities, HealthWorkflow};
use temporalio_client::{Client, ClientOptions, Connection, ConnectionOptions, WorkflowGetResultOptions, WorkflowStartOptions};
use temporalio_common::telemetry::TelemetryOptions;
use temporalio_sdk::{Worker, WorkerOptions};
use temporalio_sdk_core::ephemeral_server::{default_cached_download, TemporalDevServerConfig};
use temporalio_sdk_core::{CoreRuntime, RuntimeOptions};
use url::Url;

const TASK_QUEUE: &str = "smooth-operator-temporal-test";

#[tokio::test]
async fn health_workflow_runs_end_to_end_on_ephemeral_server() -> anyhow::Result<()> {
    // Start an ephemeral dev server (downloads + caches the CLI on first run).
    // A generous start window covers the one-time binary download.
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
            eprintln!("SKIP: ephemeral Temporal dev server did not start within 120s (likely a slow/blocked download)");
            return Ok(());
        }
    };

    let runtime_options = RuntimeOptions::builder()
        .telemetry_options(TelemetryOptions::builder().build())
        .build()
        .map_err(|e| anyhow::anyhow!("runtime options: {e}"))?;
    let runtime = CoreRuntime::new_assume_tokio(runtime_options).map_err(|e| anyhow::anyhow!("core runtime: {e}"))?;

    let target = Url::parse(&format!("http://{}", server.target))?;
    let connection = Connection::connect(ConnectionOptions::new(target).identity("smooth-operator-temporal-test".to_owned()).build()).await?;
    let client = Client::new(connection, ClientOptions::new("default").build()).map_err(|e| anyhow::anyhow!("client: {e}"))?;

    let worker_options = WorkerOptions::new(TASK_QUEUE)
        .register_workflow::<HealthWorkflow>()
        .register_activities(AgentTurnActivities)
        .build();
    let mut worker = Worker::new(&runtime, client.clone(), worker_options).map_err(|e| anyhow::anyhow!("worker: {e}"))?;
    let shutdown = worker.shutdown_handle();

    // Run the worker poll loop and the client-side execution concurrently on the
    // same task (the worker borrows `&runtime`, so it can't be `tokio::spawn`ed).
    // The client stops the worker via `shutdown` once it has the result.
    let starter = client.clone();
    let client_work = async move {
        let handle = starter
            .start_workflow(
                HealthWorkflow::run,
                "ping".to_string(),
                WorkflowStartOptions::new(TASK_QUEUE, "health-e2e-1").build(),
            )
            .await?;
        let result: String = handle.get_result(WorkflowGetResultOptions::default()).await?;
        shutdown();
        anyhow::Ok(result)
    };

    let (worker_res, client_res): (Result<(), anyhow::Error>, anyhow::Result<String>) = tokio::join!(worker.run(), client_work);
    worker_res?;
    let result = client_res?;

    assert_eq!(result, "smooth-operator-temporal ok: ping");

    let mut server = server;
    server.shutdown().await.ok();
    Ok(())
}
