//! Agent execution backend abstraction.
//!
//! An [`AgentExecutor`] is the seam that decides *where and how* an agent turn
//! runs, while [`Agent`](crate::agent::Agent) remains the unit of orchestration
//! (system prompt, tools, loop policy, compaction, checkpointing). Today there
//! is one implementation, [`InProcessExecutor`], which drives the turn directly
//! in the calling task — identical behavior to calling [`Agent::run`] /
//! [`Agent::run_with_channel`] yourself. It carries no dependencies and needs no
//! infrastructure: it is the zero-infra default and the OSS default.
//!
//! The point of the trait is to let an *optional* durable backend
//! (a Temporal-backed executor, in a separate crate, behind a feature flag and
//! off by default) plug in without the engine or its consumers knowing. Such a
//! backend models the turn as a durable workflow and runs the agent's
//! side-effects (model calls, tool invocations, retrieval, persistence) as
//! activities, giving crash-safe resume, durable human-in-the-loop via signals,
//! and durable timers — see ADR-030 (`SMOODEV-1972`). The in-process executor
//! runs those same side-effects inline; the trait boundary is what lets the two
//! be swapped at the edge.
//!
//! Keeping this as a thin dispatch seam (rather than reaching into the loop) is
//! deliberate: the in-process path stays a verbatim delegation to the existing,
//! battle-tested [`Agent::run`] surface, so introducing the abstraction changes
//! no behavior. The deeper orchestration-vs-activity split that a durable
//! backend needs lives behind this boundary and is built in the executor that
//! requires it, not forced onto the default path.

use tokio::sync::mpsc::UnboundedSender;

use async_trait::async_trait;

use crate::agent::{Agent, AgentEvent};
use crate::conversation::Conversation;

/// A backend that executes an [`Agent`] turn and returns the resulting
/// [`Conversation`].
///
/// Implementations decide *how* the turn is driven (inline, or on a durable
/// workflow engine); the [`Agent`] passed in owns *what* the turn does. Both
/// methods take `&Agent` so an executor never takes ownership of the agent — it
/// borrows it for the duration of the turn, exactly like [`Agent::run`].
#[async_trait]
pub trait AgentExecutor: Send + Sync {
    /// Run a single turn for `user_message`, returning the full conversation.
    ///
    /// Behaviorally equivalent to [`Agent::run`] for the in-process backend.
    ///
    /// # Errors
    /// Propagates any fatal error from the underlying turn (LLM call, tool
    /// execution, or — for durable backends — the workflow engine).
    async fn execute(&self, agent: &Agent, user_message: String) -> anyhow::Result<Conversation>;

    /// Run a single turn, emitting [`AgentEvent`]s over `events` as they occur
    /// (token deltas, tool start/complete, completion).
    ///
    /// Behaviorally equivalent to [`Agent::run_with_channel`] for the in-process
    /// backend.
    ///
    /// # Errors
    /// Propagates any fatal error from the underlying turn.
    async fn execute_streaming(&self, agent: &Agent, user_message: String, events: UnboundedSender<AgentEvent>) -> anyhow::Result<Conversation>;
}

/// The default, zero-infra executor: it drives the turn inline in the calling
/// task by delegating straight to [`Agent::run`] / [`Agent::run_with_channel`].
///
/// This is a verbatim pass-through — introducing it changes no behavior. It is
/// the executor used unless a consumer explicitly opts into a durable backend.
#[derive(Debug, Clone, Copy, Default)]
pub struct InProcessExecutor;

impl InProcessExecutor {
    /// Construct the in-process executor. (It is a unit type; `new` exists for
    /// symmetry with stateful executors.)
    #[must_use]
    pub fn new() -> Self {
        Self
    }
}

#[async_trait]
impl AgentExecutor for InProcessExecutor {
    async fn execute(&self, agent: &Agent, user_message: String) -> anyhow::Result<Conversation> {
        agent.run(user_message).await
    }

    async fn execute_streaming(&self, agent: &Agent, user_message: String, events: UnboundedSender<AgentEvent>) -> anyhow::Result<Conversation> {
        agent.run_with_channel(user_message, events).await
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use super::*;
    use crate::agent::AgentConfig;
    use crate::llm::{LlmConfig, StreamEvent};
    use crate::llm_provider::MockLlmClient;
    use crate::tool::ToolRegistry;

    fn test_config() -> AgentConfig {
        AgentConfig::new("test-agent", "You are a test agent", LlmConfig::openrouter("fake-key"))
    }

    /// The in-process executor drives the loop identically to `Agent::run`:
    /// a single text response with no tool calls ends the turn after one LLM
    /// call, with the assistant content the mock scripted.
    #[tokio::test]
    async fn in_process_executor_matches_agent_run() {
        let mock = MockLlmClient::new();
        mock.push_text("the answer is 42");
        let agent = Agent::new(test_config(), ToolRegistry::new()).with_llm_provider(Arc::new(mock.clone()));

        let convo = InProcessExecutor::new()
            .execute(&agent, "what is the answer?".to_string())
            .await
            .expect("execute completes");

        assert_eq!(convo.last_assistant_content(), Some("the answer is 42"));
        assert_eq!(mock.call_count(), 1);
        let calls = mock.calls();
        assert!(calls[0].messages.iter().any(|m| m.content.contains("what is the answer?")));
    }

    /// The streaming entry point surfaces events over the channel and returns
    /// the same final conversation.
    #[tokio::test]
    async fn in_process_executor_streaming_emits_events_and_returns_conversation() {
        // The streaming path drives `chat_stream`, which the mock services from
        // its `streams` queue — script it with deltas + a terminal Done.
        let mock = MockLlmClient::new();
        mock.push_stream(vec![
            StreamEvent::Delta { content: "streamed ".into() },
            StreamEvent::Delta { content: "reply".into() },
            StreamEvent::Done { finish_reason: "stop".into() },
        ]);
        let agent = Agent::new(test_config(), ToolRegistry::new()).with_llm_provider(Arc::new(mock.clone()));

        let (tx, mut rx) = tokio::sync::mpsc::unbounded_channel();
        let convo = InProcessExecutor::new()
            .execute_streaming(&agent, "stream please".to_string(), tx)
            .await
            .expect("streaming execute completes");

        assert_eq!(convo.last_assistant_content(), Some("streamed reply"));

        // At least one event was emitted (Started, ... Completed).
        let mut events = Vec::new();
        while let Ok(ev) = rx.try_recv() {
            events.push(ev);
        }
        assert!(!events.is_empty(), "expected streaming events to be emitted");
    }

    /// A dyn-dispatched executor works — the abstraction is object-safe, which
    /// is what lets a consumer hold `Arc<dyn AgentExecutor>` and swap backends.
    #[tokio::test]
    async fn executor_is_object_safe() {
        let mock = MockLlmClient::new();
        mock.push_text("dyn ok");
        let agent = Agent::new(test_config(), ToolRegistry::new()).with_llm_provider(Arc::new(mock.clone()));

        let executor: Arc<dyn AgentExecutor> = Arc::new(InProcessExecutor::new());
        let convo = executor.execute(&agent, "via dyn".to_string()).await.expect("dyn execute completes");

        assert_eq!(convo.last_assistant_content(), Some("dyn ok"));
    }
}
