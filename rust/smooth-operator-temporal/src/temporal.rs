//! Temporal-backed durable execution (feature `temporal`).
//!
//! Spike scaffold: a trivial workflow + activity proving the preview SDK
//! (`temporalio-sdk 0.4`) integrates in this crate. The real agent activities
//! (`model_call`, `tool_invoke`) and the `drive_turn`-driven `AgentTurnWorkflow`
//! build on this.

use std::time::Duration;

use temporalio_macros::{activities, workflow, workflow_methods};
use temporalio_sdk::activities::{ActivityContext, ActivityError};
use temporalio_sdk::{ActivityOptions, WorkflowContext, WorkflowResult};

/// Activities for the agent-turn workflow. Holds no per-instance state — the
/// SDK registers the methods as free functions (mirroring the platform
/// temporal-worker), so engine handles come from process-level init.
pub struct AgentTurnActivities;

#[activities]
impl AgentTurnActivities {
    /// Liveness probe used by the scaffold workflow.
    #[activity]
    pub async fn health_echo(_ctx: ActivityContext, message: String) -> Result<String, ActivityError> {
        Ok(format!("smooth-operator-temporal ok: {message}"))
    }
}

/// Scaffold liveness workflow — proves the SDK integrates end to end.
#[workflow]
#[derive(Default)]
pub struct HealthWorkflow;

#[workflow_methods]
impl HealthWorkflow {
    #[run]
    pub async fn run(ctx: &mut WorkflowContext<Self>, message: String) -> WorkflowResult<String> {
        let echoed = ctx
            .start_activity(
                AgentTurnActivities::health_echo,
                message,
                ActivityOptions::start_to_close_timeout(Duration::from_secs(10)),
            )
            .await
            .map_err(|e| anyhow::anyhow!("{e}"))?;
        Ok(echoed)
    }
}
