//! Temporal-backed durable execution (feature `temporal`).
//!
//! An agent turn runs as the [`AgentTurnWorkflow`], whose side-effects — the
//! model call and each tool invocation — are Temporal **activities**
//! ([`AgentTurnActivities`]). The workflow drives the engine's deterministic
//! [`drive_turn`] orchestration **unchanged** via a [`WorkflowAgentActivities`]
//! adapter that schedules those activities on the [`WorkflowContext`]. The
//! in-process executor runs the same `drive_turn` inline — one loop, two
//! backends.
//!
//! ## Engine handles
//!
//! Temporal registers activity methods as free functions (no per-instance
//! state), so the model provider + tool registry the activities need are held in
//! a process-global set once at worker startup via [`init_engine`] — mirroring
//! the platform temporal-worker's lazy DB pool. A misconfigured worker (handles
//! never initialized) fails activities loudly as non-retryable, rather than
//! silently no-oping.

use std::sync::{Arc, OnceLock};
use std::time::Duration;

use async_trait::async_trait;
use temporalio_common::error::ApplicationFailure;
use temporalio_macros::{activities, workflow, workflow_methods};
use temporalio_sdk::activities::{ActivityContext, ActivityError};
use temporalio_sdk::{ActivityOptions, SyncWorkflowContext, WorkflowContext, WorkflowResult};

use smooth_operator_core::activities::{drive_turn, AgentActivities, TurnPolicy};
use smooth_operator_core::conversation::{Conversation, Message};
use smooth_operator_core::llm::LlmResponse;
use smooth_operator_core::llm_provider::LlmProvider;
use smooth_operator_core::tool::{ToolCall, ToolRegistry, ToolResult, ToolSchema};

use crate::dto::{ModelCallInput, ModelCallOutput, ToolInvokeInput};

/// Process-level engine handles the activities run against. Set once at worker
/// startup with [`init_engine`].
pub struct EngineHandles {
    /// Model provider backing the `model_call` activity.
    pub llm: Arc<dyn LlmProvider>,
    /// Tool registry backing the `tool_invoke` activity.
    pub tools: Arc<ToolRegistry>,
}

static ENGINE: OnceLock<EngineHandles> = OnceLock::new();

/// Install the engine handles the Temporal activities run against. Call once,
/// before starting the worker. Returns `Err` (with the rejected handles) if
/// already initialized.
///
/// # Errors
/// Returns the passed-in handles back if the global was already set.
pub fn init_engine(handles: EngineHandles) -> Result<(), EngineHandles> {
    ENGINE.set(handles)
}

fn engine() -> Result<&'static EngineHandles, ActivityError> {
    ENGINE.get().ok_or_else(|| {
        ActivityError::application(ApplicationFailure::non_retryable(anyhow::anyhow!(
            "engine handles not initialized — call smooth_operator_temporal::temporal::init_engine() at worker startup"
        )))
    })
}

/// Default context-token budget for a workflow-seeded conversation. Mirrors
/// `AgentConfig::new`'s default.
const DEFAULT_MAX_CONTEXT_TOKENS: usize = 100_000;

/// Activities for the agent-turn workflow. A unit struct — the SDK registers the
/// methods as free functions; their state comes from the [`ENGINE`] global.
pub struct AgentTurnActivities;

#[activities]
impl AgentTurnActivities {
    /// Liveness probe used by the scaffold [`HealthWorkflow`].
    #[activity]
    pub async fn health_echo(_ctx: ActivityContext, message: String) -> Result<String, ActivityError> {
        Ok(format!("smooth-operator-temporal ok: {message}"))
    }

    /// The model call: the `Think` step of the loop, run as a durable activity.
    #[activity]
    pub async fn model_call(_ctx: ActivityContext, input: ModelCallInput) -> Result<ModelCallOutput, ActivityError> {
        let engine = engine()?;
        let refs: Vec<&Message> = input.messages.iter().collect();
        let response = engine
            .llm
            .chat(&refs, &input.tools)
            .await
            .map_err(|e| ActivityError::application(ApplicationFailure::new(anyhow::anyhow!("model_call: {e}"))))?;
        Ok(ModelCallOutput::from(&response))
    }

    /// A single tool invocation: the `Act` step, run as a durable activity. Tool
    /// *business* failures come back in [`ToolResult::is_error`] (not as `Err`),
    /// matching [`ToolRegistry::execute`].
    #[activity]
    pub async fn tool_invoke(_ctx: ActivityContext, input: ToolInvokeInput) -> Result<ToolResult, ActivityError> {
        let engine = engine()?;
        Ok(engine.tools.execute(&input.call).await)
    }
}

/// Activity options for the model/tool activities: 60s start-to-close, mirroring
/// the platform worker's defaults. (Retry-policy tuning lands with the per-step
/// retry work.)
fn agent_activity_opts() -> ActivityOptions {
    ActivityOptions::start_to_close_timeout(Duration::from_secs(60))
}

/// Adapter that makes the engine's [`AgentActivities`] surface schedule Temporal
/// activities on a [`WorkflowContext`]. Holding `&WorkflowContext` (a `!Send`
/// `Rc<RefCell<…>>` internally) is why [`AgentActivities`] is `?Send`.
///
/// `approval_required` names the tools that need human approval before they run:
/// `tool_invoke` blocks **durably** on an approval signal for those, resuming
/// across worker restarts (the HITL unlock — no mid-turn connection state, just a
/// signal).
struct WorkflowAgentActivities<'a> {
    ctx: &'a WorkflowContext<AgentTurnWorkflow>,
    approval_required: Vec<String>,
}

#[async_trait(?Send)]
impl AgentActivities for WorkflowAgentActivities<'_> {
    async fn model_call(&self, messages: Vec<Message>, tools: Vec<ToolSchema>) -> anyhow::Result<LlmResponse> {
        let output: ModelCallOutput = self
            .ctx
            .start_activity(AgentTurnActivities::model_call, ModelCallInput { messages, tools }, agent_activity_opts())
            .await
            .map_err(|e| anyhow::anyhow!("model_call activity: {e}"))?;
        Ok(output.into_llm_response())
    }

    async fn tool_invoke(&self, call: ToolCall) -> anyhow::Result<ToolResult> {
        // Durable human-in-the-loop gate: if this tool needs approval, block
        // until an `approve_tool` / `deny_tool` signal names this call id. The
        // wait is recorded in workflow history, so it survives restarts and can
        // resolve hours later. `drive_turn` is unchanged — the gate lives here.
        if self.approval_required.iter().any(|name| name == &call.name) {
            let pending_id = call.id.clone();
            self.ctx
                .wait_condition(move |wf: &AgentTurnWorkflow| wf.approved.contains(&pending_id) || wf.denied.contains(&pending_id))
                .await;

            let denied_id = call.id.clone();
            if self.ctx.state(move |wf: &AgentTurnWorkflow| wf.denied.contains(&denied_id)) {
                // Denied: surface a tool-error result so the model can react,
                // without ever executing the tool.
                return Ok(ToolResult {
                    tool_call_id: call.id.clone(),
                    content: format!("Tool call '{}' was denied by human approval.", call.name),
                    is_error: true,
                    details: None,
                });
            }
        }

        let result = self
            .ctx
            .start_activity(AgentTurnActivities::tool_invoke, ToolInvokeInput { call }, agent_activity_opts())
            .await
            .map_err(|e| anyhow::anyhow!("tool_invoke activity: {e}"))?;
        Ok(result)
    }
}

/// Input to [`AgentTurnWorkflow`]: everything needed to seed the conversation and
/// bound the loop. Serializable so it crosses the workflow-start boundary.
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
pub struct AgentTurnInput {
    /// System prompt for the turn.
    pub system_prompt: String,
    /// The user message that opens the turn.
    pub user_message: String,
    /// Tool schemas available to the model.
    #[serde(default)]
    pub tools: Vec<ToolSchema>,
    /// Iteration bound. `0` falls back to the [`TurnPolicy`] default.
    #[serde(default)]
    pub max_iterations: u32,
    /// Names of tools that require human approval before they run. When the
    /// model calls one of these, the workflow blocks durably until an
    /// `approve_tool` / `deny_tool` signal names that tool call.
    #[serde(default)]
    pub approval_required_tools: Vec<String>,
}

/// The agent turn as a Temporal workflow. Seeds the conversation, then drives the
/// engine's [`drive_turn`] unchanged over [`WorkflowAgentActivities`], so the
/// durable path is the same loop as in-process. Returns the full conversation.
///
/// Workflow state is the human-approval ledger: `approved` / `denied` tool-call
/// ids, populated by the `approve_tool` / `deny_tool` signals and observed by the
/// adapter's durable gate.
#[workflow]
#[derive(Default)]
pub struct AgentTurnWorkflow {
    /// Tool-call ids a human has approved.
    approved: Vec<String>,
    /// Tool-call ids a human has denied.
    denied: Vec<String>,
}

#[workflow_methods]
impl AgentTurnWorkflow {
    #[run]
    pub async fn run(ctx: &mut WorkflowContext<Self>, input: AgentTurnInput) -> WorkflowResult<Conversation> {
        let mut conversation = Conversation::new(DEFAULT_MAX_CONTEXT_TOKENS).with_system_prompt(&input.system_prompt);
        conversation.push(Message::user(input.user_message));

        let policy = if input.max_iterations == 0 {
            TurnPolicy::default()
        } else {
            TurnPolicy {
                max_iterations: input.max_iterations,
            }
        };

        let adapter = WorkflowAgentActivities {
            ctx: &*ctx,
            approval_required: input.approval_required_tools,
        };
        drive_turn(&adapter, &mut conversation, input.tools, &policy)
            .await
            .map_err(|e| anyhow::anyhow!("drive_turn: {e}"))?;

        Ok(conversation)
    }

    /// Signal: a human approves the tool call with this id, unblocking the gate.
    #[signal]
    pub fn approve_tool(&mut self, _ctx: &mut SyncWorkflowContext<Self>, call_id: String) {
        if !self.approved.contains(&call_id) {
            self.approved.push(call_id);
        }
    }

    /// Signal: a human denies the tool call with this id; the gate returns a
    /// tool-error result instead of running it.
    #[signal]
    pub fn deny_tool(&mut self, _ctx: &mut SyncWorkflowContext<Self>, call_id: String) {
        if !self.denied.contains(&call_id) {
            self.denied.push(call_id);
        }
    }
}

/// Scaffold liveness workflow — proves the SDK integrates end to end and backs
/// the ephemeral-server integration test.
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
