//! Deterministic turn orchestration over a pluggable I/O surface.
//!
//! This is the keystone of the fine-grained durable executor (ADR-030,
//! `SMOODEV-1974`). It factors an agent turn into two halves:
//!
//! - [`AgentActivities`] — the *side-effecting* boundary: the model call and
//!   each tool invocation. These are exactly the steps a durable backend runs as
//!   Temporal **activities** (retried, memoized in history). Inputs and outputs
//!   are owned, serde-friendly values so they can cross an activity boundary
//!   unchanged.
//! - [`drive_turn`] — the *deterministic orchestration*: the loop that decides
//!   "call the model → if it asked for tools, run them and loop → else stop." It
//!   contains no I/O, no wall-clock, and no randomness, so it is replay-safe: a
//!   Temporal workflow can run this very function, and the in-process executor
//!   runs it too. **One loop, two backends** — which is what keeps the durable
//!   path from diverging from the inline path.
//!
//! [`InProcessActivities`] is the zero-infra implementation, backed by the
//! existing [`LlmProvider`] and [`ToolRegistry`] seams; it runs the side-effects
//! inline. The Temporal-backed implementation (a separate, feature-gated crate)
//! implements the same trait by scheduling activities on a [`WorkflowContext`],
//! then calls `drive_turn` unchanged — see `SMOODEV-1974`.
//!
//! ## Scope of this orchestration vs. `Agent::run`
//!
//! `drive_turn` reproduces the *core decision flow* of [`Agent::run`]
//! (`agent.rs`): context window → model call → append assistant message →
//! stop-or-run-tools → append tool results → loop to `max_iterations`. It
//! deliberately omits the inline-runtime concerns that a durable backend models
//! differently or that are follow-ups in the epic:
//!
//! - **Event emission** (`AgentEvent`) — an inline-UI concern; durable backends
//!   surface progress from workflow history / queries instead.
//! - **Checkpointing** — Temporal's event history *is* the checkpoint; the
//!   in-process path keeps using [`CheckpointStore`](crate::checkpoint) via
//!   `Agent::run`. (`SMOODEV-1977` reconciles the two.)
//! - **Proactive/reactive compaction, budget enforcement, parallel tools, the
//!   max-steps reminder, knowledge/memory injection** — tracked follow-ups; they
//!   layer onto this loop without changing its shape.
//!
//! The convergence goal (tracked under the epic) is for `Agent::run` itself to
//! delegate to `drive_turn` once these are folded in, so there is a single
//! orchestration with two execution backends.

use std::sync::Arc;

use async_trait::async_trait;

use crate::conversation::{Conversation, Message};
use crate::llm::LlmResponse;
use crate::llm_provider::LlmProvider;
use crate::tool::{ToolCall, ToolRegistry, ToolResult, ToolSchema};

/// The side-effecting boundary of an agent turn: the model call and tool
/// invocations. A durable backend runs each of these as a Temporal activity; the
/// in-process backend runs them inline.
///
/// Inputs/outputs are owned values (not borrows) so an implementation may
/// serialize them across an activity boundary.
///
/// The trait is `?Send` (no `Send`/`Sync` bound, non-`Send` futures): a Temporal
/// workflow-backed implementation drives the single-threaded, `!Send`
/// `WorkflowContext` (`Rc<RefCell<…>>` internally), so requiring `Send` would make
/// `drive_turn` uncallable from workflow code. The in-process path awaits
/// `drive_turn` directly (never across threads), so it is unaffected.
#[async_trait(?Send)]
pub trait AgentActivities {
    /// Invoke the model with the given context and available tool schemas.
    ///
    /// # Errors
    /// Propagates a fatal model-call error (network, upstream rejection). A
    /// durable backend turns transient failures into activity retries before
    /// this surfaces.
    async fn model_call(&self, messages: Vec<Message>, tools: Vec<ToolSchema>) -> anyhow::Result<LlmResponse>;

    /// Execute a single tool call, returning its result.
    ///
    /// # Errors
    /// Propagates a fatal dispatch error. Note tool *business* failures are
    /// reported in [`ToolResult::is_error`], not as `Err` — mirroring
    /// [`ToolRegistry::execute`].
    async fn tool_invoke(&self, call: ToolCall) -> anyhow::Result<ToolResult>;
}

/// Loop policy for [`drive_turn`]: the bound that keeps the orchestration
/// terminating. Mirrors the `max_iterations` field of
/// [`AgentConfig`](crate::agent::AgentConfig).
#[derive(Debug, Clone, Copy)]
pub struct TurnPolicy {
    /// Maximum model-call iterations before the turn stops unconditionally.
    pub max_iterations: u32,
}

impl Default for TurnPolicy {
    fn default() -> Self {
        // Matches `AgentConfig::new`'s default.
        Self { max_iterations: 50 }
    }
}

/// Drive one agent turn deterministically over the supplied activity surface,
/// mutating `conversation` in place.
///
/// `conversation` must already be seeded with the system prompt, any prior
/// turns, and the current user message — exactly the state `Agent::run` holds
/// when it enters its loop. On return, `conversation` carries the appended
/// assistant/tool messages.
///
/// This function performs **no I/O, wall-clock reads, or RNG** of its own — all
/// of that is delegated to `activities` — so it is safe to run as Temporal
/// workflow code (replay-deterministic) as well as inline.
///
/// # Errors
/// Propagates the first fatal error from [`AgentActivities::model_call`] or
/// [`AgentActivities::tool_invoke`].
pub async fn drive_turn(activities: &dyn AgentActivities, conversation: &mut Conversation, tools: Vec<ToolSchema>, policy: &TurnPolicy) -> anyhow::Result<()> {
    for _iteration in 1..=policy.max_iterations {
        // Observe: snapshot the context window as owned messages so the call can
        // cross an activity boundary.
        let context: Vec<Message> = conversation.context_window().into_iter().cloned().collect();

        // Think.
        let response = activities.model_call(context, tools.clone()).await?;

        // Append the assistant turn (carrying any tool calls + reasoning),
        // mirroring `Agent::run`'s push condition exactly.
        if !response.content.is_empty() || !response.tool_calls.is_empty() || response.reasoning_content.is_some() {
            let mut msg = Message::assistant(&response.content);
            msg.tool_calls.clone_from(&response.tool_calls);
            msg.reasoning_content.clone_from(&response.reasoning_content);
            conversation.push(msg);
        }

        // No tool calls ⇒ the agent is done.
        if response.tool_calls.is_empty() {
            return Ok(());
        }

        // Act: run each tool and append its result, paired by id + name so the
        // model can match results to calls.
        for call in &response.tool_calls {
            let result = activities.tool_invoke(call.clone()).await?;
            conversation.push(Message::tool_result_named(&call.id, &call.name, &result.content));
        }
    }

    // Iteration budget exhausted mid-tool-chain — return what we have, matching
    // `Agent::run`'s post-loop behavior.
    Ok(())
}

/// The default, zero-infra [`AgentActivities`]: runs the model call through an
/// [`LlmProvider`] and tool calls through a [`ToolRegistry`], inline in the
/// calling task.
pub struct InProcessActivities {
    llm: Arc<dyn LlmProvider>,
    tools: ToolRegistry,
}

impl InProcessActivities {
    /// Build an in-process activity surface from a model provider and tool
    /// registry.
    #[must_use]
    pub fn new(llm: Arc<dyn LlmProvider>, tools: ToolRegistry) -> Self {
        Self { llm, tools }
    }
}

#[async_trait(?Send)]
impl AgentActivities for InProcessActivities {
    async fn model_call(&self, messages: Vec<Message>, tools: Vec<ToolSchema>) -> anyhow::Result<LlmResponse> {
        let refs: Vec<&Message> = messages.iter().collect();
        self.llm.chat(&refs, &tools).await
    }

    async fn tool_invoke(&self, call: ToolCall) -> anyhow::Result<ToolResult> {
        // `ToolRegistry::execute` reports tool failures via `ToolResult::is_error`
        // rather than `Err`, so dispatch itself is infallible here.
        Ok(self.tools.execute(&call).await)
    }
}

#[cfg(test)]
mod tests {
    use async_trait::async_trait;

    use super::*;
    use crate::agent::{Agent, AgentConfig};
    use crate::conversation::Role;
    use crate::llm::LlmConfig;
    use crate::llm_provider::MockLlmClient;
    use crate::tool::{Tool, ToolRegistry, ToolSchema};

    /// Minimal tool that echoes its `text` argument — mirrors the in-crate
    /// `EchoTool` used elsewhere.
    struct EchoTool;

    #[async_trait]
    impl Tool for EchoTool {
        fn schema(&self) -> ToolSchema {
            ToolSchema {
                name: "echo".into(),
                description: "Echoes input back".into(),
                parameters: serde_json::json!({
                    "type": "object",
                    "properties": { "text": {"type": "string"} },
                    "required": ["text"]
                }),
            }
        }

        async fn execute(&self, arguments: serde_json::Value) -> anyhow::Result<String> {
            Ok(arguments["text"].as_str().unwrap_or("").to_string())
        }
    }

    fn seed_conversation(user: &str) -> Conversation {
        let mut c = Conversation::new(100_000).with_system_prompt("You are a test agent");
        c.push(Message::user(user));
        c
    }

    /// A plain text reply ends the turn after exactly one model call, with the
    /// assistant content the mock scripted — no tools run.
    #[tokio::test]
    async fn drive_turn_text_reply_stops_after_one_model_call() {
        let mock = MockLlmClient::new();
        mock.push_text("the answer is 42");
        let activities = InProcessActivities::new(Arc::new(mock.clone()), ToolRegistry::new());

        let mut convo = seed_conversation("what is the answer?");
        drive_turn(&activities, &mut convo, vec![], &TurnPolicy::default())
            .await
            .expect("turn completes");

        assert_eq!(convo.last_assistant_content(), Some("the answer is 42"));
        assert_eq!(mock.call_count(), 1);
    }

    /// A tool call is executed and its result appended, then the follow-up text
    /// reply ends the turn — two model calls, one tool result in between.
    #[tokio::test]
    async fn drive_turn_runs_tool_then_finishes() {
        let mock = MockLlmClient::new();
        mock.push_tool_call("call-1", "echo", serde_json::json!({ "text": "hello tools" }));
        mock.push_text("done");
        let mut registry = ToolRegistry::new();
        registry.register(EchoTool);
        let activities = InProcessActivities::new(Arc::new(mock.clone()), registry);

        let mut convo = seed_conversation("use the echo tool");
        drive_turn(&activities, &mut convo, vec![], &TurnPolicy::default())
            .await
            .expect("turn completes");

        assert_eq!(mock.call_count(), 2);
        // The tool result landed as a Tool-role message carrying the echoed text.
        let tool_msgs: Vec<&Message> = convo.messages.iter().filter(|m| m.role == Role::Tool).collect();
        assert_eq!(tool_msgs.len(), 1);
        assert_eq!(tool_msgs[0].content, "hello tools");
        assert_eq!(tool_msgs[0].tool_name.as_deref(), Some("echo"));
        assert_eq!(convo.last_assistant_content(), Some("done"));
    }

    /// `drive_turn` produces the same message tail as `Agent::run` for an
    /// identical script + tools — proving the shared orchestration matches the
    /// battle-tested inline loop (the anti-divergence guarantee).
    #[tokio::test]
    async fn drive_turn_matches_agent_run_message_sequence() {
        // Two independent mocks with identical FIFO scripts.
        let script = |m: &MockLlmClient| {
            m.push_tool_call("call-1", "echo", serde_json::json!({ "text": "ping" }));
            m.push_text("final");
        };

        // --- Agent::run path ---
        let agent_mock = MockLlmClient::new();
        script(&agent_mock);
        let mut agent_registry = ToolRegistry::new();
        agent_registry.register(EchoTool);
        let agent = Agent::new(
            AgentConfig::new("test-agent", "You are a test agent", LlmConfig::openrouter("fake-key")),
            agent_registry,
        )
        .with_llm_provider(Arc::new(agent_mock.clone()));
        let agent_convo = agent.run("use echo").await.expect("agent run completes");

        // --- drive_turn path, seeded identically (system + user) ---
        let dt_mock = MockLlmClient::new();
        script(&dt_mock);
        let mut dt_registry = ToolRegistry::new();
        dt_registry.register(EchoTool);
        let activities = InProcessActivities::new(Arc::new(dt_mock.clone()), dt_registry);
        let mut dt_convo = seed_conversation("use echo");
        drive_turn(&activities, &mut dt_convo, vec![], &TurnPolicy::default())
            .await
            .expect("drive_turn completes");

        // Same model-call count and same (role, content, tool_name) tail.
        assert_eq!(agent_mock.call_count(), dt_mock.call_count(), "model-call counts diverge");

        let tail = |c: &Conversation| -> Vec<(Role, String, Option<String>)> {
            c.messages.iter().map(|m| (m.role.clone(), m.content.clone(), m.tool_name.clone())).collect()
        };
        assert_eq!(
            tail(&agent_convo),
            tail(&dt_convo),
            "message sequences diverge between Agent::run and drive_turn"
        );
    }

    /// The activity surface is object-safe behind `Arc<dyn AgentActivities>`,
    /// which is how the executor holds it.
    #[tokio::test]
    async fn activities_are_object_safe() {
        let mock = MockLlmClient::new();
        mock.push_text("ok");
        let activities: Arc<dyn AgentActivities> = Arc::new(InProcessActivities::new(Arc::new(mock.clone()), ToolRegistry::new()));

        let mut convo = seed_conversation("hi");
        drive_turn(activities.as_ref(), &mut convo, vec![], &TurnPolicy::default())
            .await
            .expect("turn completes");
        assert_eq!(convo.last_assistant_content(), Some("ok"));
    }
}
