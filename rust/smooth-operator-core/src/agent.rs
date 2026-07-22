use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use tokio::sync::mpsc::{UnboundedReceiver, UnboundedSender};

use crate::checkpoint::{Checkpoint, CheckpointEvent, CheckpointStore, CheckpointStrategy};
use crate::cost::{CostBudget, CostTracker, ModelPricing};
use crate::human::{HumanRequest, HumanResponse};
use crate::knowledge::KnowledgeBase;
use crate::memory::Memory;
use futures_util::StreamExt;

use crate::conversation::{CompactionStrategy, Conversation, Message, ReactiveCompaction, Role};
use crate::llm::{accumulate_stream_events, LlmClient, LlmConfig, StreamEvent};
use crate::llm_provider::LlmProvider;
use crate::tool::{Tool, ToolRegistry, ToolSchema};

/// Reminder injected as a system message on the final iteration of the agent
/// loop. Adapted from opencode's `max-steps.txt` system reminder. Lets the
/// model wrap up cleanly — what's done, what's left, what to do next — instead
/// of being cut off mid-tool-call when the iteration cap fires.
const MAX_STEPS_REMINDER: &str = "MAXIMUM ITERATIONS REACHED. This is your final iteration before the loop ends. \
Do not start new tool chains. Respond with text only:\n\
1. State that the maximum iterations for this agent have been reached.\n\
2. Summarize what has been accomplished so far.\n\
3. List any remaining tasks that were not completed.\n\
4. Recommend what should be done next (a follow-up dispatch, a manual step, a question for the user).";

/// System prompt for the fast-model preamble (see [`AgentConfig::preamble`]).
/// Deliberately narrow: one short present-tense sentence, no answer, no
/// greeting, no promises — it's generated WITHOUT the tool result, so it must
/// describe intent only.
const PREAMBLE_SYSTEM_PROMPT: &str = "You are the assistant's voice while it works. \
In ONE short present-tense sentence (max ~12 words), tell the user what you're about to do to help with their message. \
Do NOT answer the question, do NOT greet, do NOT promise a specific result or outcome. \
Example: \"Let me pull up your recent conversations.\" \
Reply with only that sentence — no quotes, no preamble, no markdown.";

/// Verify-tests-before-done system-prompt rule. Appended by
/// [`AgentConfig::with_verify_tests_before_done`]. The anchor lets the
/// builder detect a prior append and avoid double-stacking the rule.
/// Pearl th-operator-verify-rule (sub-pearl of th-VERIFY-PHASE).
pub const VERIFY_TESTS_RULE_ANCHOR: &str = "[verify-tests-before-done:v1]";

/// Body of the verify-tests rule. Kept narrow on purpose: don't try to
/// override the agent's normal completion logic, just gate the
/// terminal "I'm done" message on having actually seen passing tests.
/// The agent is free to skip the test run if the task genuinely
/// doesn't have tests — the rule is "if tests exist, you must run
/// them," not "you must run tests even when there are none."
pub const VERIFY_TESTS_RULE: &str = "[verify-tests-before-done:v1] \
This task is being scored against a test suite. You MUST NOT produce a final response (or stop iterating) until you have:\n\
1. Run the project's test command at least once. The typical commands are:\n\
   - Python: `pytest -q` (or `python3 -m pytest -q`)\n\
   - Rust: `cargo test` (with `-- --include-ignored` if the workspace uses ignored tests)\n\
   - JS / TS: `npm test` or `pnpm test`\n\
   - Go: `go test ./...`\n\
   Pick the one that matches the project files you can see.\n\
2. Seen all tests pass.\n\n\
If tests are failing after a run, your ONLY valid next action is to fix the failures and re-run the test command. Do NOT summarize. Do NOT declare done. Do NOT say things like \"the implementation should work\" or \"all tests should pass now\" — re-run the tests and quote the actual output.\n\n\
The test-runner output (exit code + summary line) is the authoritative completion signal. Your own assessment of correctness is not.\n";

/// Configuration for an agent.
#[allow(missing_debug_implementations)]
pub struct AgentConfig {
    pub name: String,
    pub system_prompt: String,
    pub llm: LlmConfig,
    pub max_iterations: u32,
    pub max_context_tokens: usize,
    pub checkpoint_strategy: CheckpointStrategy,
    pub compaction_strategy: CompactionStrategy,
    pub parallel_tools: bool,
    pub memory: Option<Arc<dyn Memory>>,
    pub knowledge: Option<Arc<dyn KnowledgeBase>>,
    pub budget: Option<CostBudget>,
    pub human_tx: Option<UnboundedSender<HumanRequest>>,
    pub human_rx: Option<Arc<tokio::sync::Mutex<UnboundedReceiver<HumanResponse>>>>,
    /// Optional injection channel — out-of-band messages (mailbox: `[CHAT:USER]`,
    /// `[STEERING:GUIDANCE]`, `[ANSWER:*]`) drained at the top of each iteration
    /// and pushed into the conversation as user-turns. Lets a host process talk
    /// to a running agent without restarting its loop.
    pub chat_rx: Option<Arc<tokio::sync::Mutex<UnboundedReceiver<InjectedMessage>>>>,
    /// Pre-existing conversation messages to seed the agent's
    /// `Conversation` with before the current user message. Lets a
    /// host inject prior chat-session turns so the agent has continuity
    /// across dispatches. Each entry pushes as a native role-tagged
    /// `Message`; tool calls / tool results are not preserved at
    /// this layer (prose only — that's a future extension).
    pub prior_messages: Vec<Message>,
    /// Image attachments to attach to the CURRENT user message (the one
    /// pushed at the start of `run`/`run_with_channel`). Set by a host
    /// that received a multimodal chat turn; consumed once, on that turn.
    /// Empty for text-only turns. Pearl th-25ce5c.
    pub next_user_images: Vec<crate::conversation::ImageContent>,
    /// The active model's hard output ceiling (`max_output_tokens`), when known.
    /// Threaded onto the built [`LlmClient`] so requests clamp `max_tokens` to
    /// `min(llm.max_tokens, ceiling)` — a budget tuned high (or resolved per-org
    /// via `@smooai/config` limits) can never exceed what the model can emit.
    /// `None` = unknown → no clamp. The host sources it per-turn from the
    /// gateway's `/model/info` for the resolved model. (EPIC th-1cc9fa.)
    pub model_max_output: Option<u32>,
    /// Optional fast-model "preamble". Reasoning models behind a gateway sit on
    /// dead air during their time-to-first-token (reasoning + tool call). When
    /// set, the FIRST turn fires this small fast model *in parallel* with the
    /// main model and streams ONE short user-facing sentence describing what's
    /// about to happen (`AgentEvent::PreambleDelta`), so the UX doesn't stall.
    /// Off unless set — every existing consumer is unaffected. Pearl th-9a5794.
    ///
    // ponytail: Rust reference impl only. The TS/Go/.NET/Python core ports skip
    // this until a non-Rust consumer needs it — every SmooAI production brain
    // (chat-ws, voice, HeyPage, general-agent→Rust) runs on the Rust core.
    pub preamble: Option<PreambleConfig>,
}

/// Config for the parallel fast-model preamble (see [`AgentConfig::preamble`]).
#[derive(Debug, Clone)]
pub struct PreambleConfig {
    /// Fast model alias to generate the preamble, e.g. `groq-gpt-oss-20b`.
    /// Resolved through the same gateway/key as the main model.
    pub model: String,
    /// Max tokens for the preamble. Keep it to one sentence. Default 64.
    pub max_tokens: u32,
}

impl PreambleConfig {
    pub fn new(model: impl Into<String>) -> Self {
        Self { model: model.into(), max_tokens: 64 }
    }
}

/// A message injected into a running agent's conversation from outside the loop.
/// Carried over `AgentConfig::chat_rx`. The `kind` controls how the message
/// is framed when pushed onto the conversation; `body` is the verbatim text.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InjectedMessage {
    pub kind: InjectedMessageKind,
    pub body: String,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum InjectedMessageKind {
    /// Direct chat from the user — pushed verbatim as a `Message::user`.
    UserChat,
    /// `[STEERING:GUIDANCE]` from the lead — pushed as a user-turn prefixed
    /// with "Lead guidance:" so the agent treats it as authoritative redirection.
    LeadGuidance,
    /// `[ANSWER:USER]` or `[ANSWER:SMOOTH]` reply to a prior `ask_smooth` —
    /// pushed as a user-turn prefixed with "Answer to your question:".
    AnswerToQuestion,
}

impl AgentConfig {
    pub fn new(name: impl Into<String>, system_prompt: impl Into<String>, llm: LlmConfig) -> Self {
        Self {
            name: name.into(),
            system_prompt: system_prompt.into(),
            llm,
            max_iterations: 50,
            max_context_tokens: 100_000,
            checkpoint_strategy: CheckpointStrategy::default(),
            compaction_strategy: CompactionStrategy::default(),
            parallel_tools: false,
            memory: None,
            knowledge: None,
            budget: None,
            human_tx: None,
            human_rx: None,
            chat_rx: None,
            prior_messages: Vec::new(),
            next_user_images: Vec::new(),
            model_max_output: None,
            preamble: None,
        }
    }

    /// Enable the parallel fast-model preamble (see [`AgentConfig::preamble`]).
    /// `None` (the default) leaves it off — no extra LLM call, no behavior change.
    #[must_use]
    pub fn with_preamble(mut self, preamble: Option<PreambleConfig>) -> Self {
        self.preamble = preamble;
        self
    }

    /// Pin the active model's output ceiling (`max_output_tokens`). The built
    /// [`LlmClient`] clamps `max_tokens` to `min(llm.max_tokens, ceiling)`.
    /// `None` leaves it unclamped (the default).
    #[must_use]
    pub fn with_model_ceiling(mut self, ceiling: Option<u32>) -> Self {
        self.model_max_output = ceiling;
        self
    }

    /// Attach image(s) to the current turn's user message. The host sets
    /// this when a chat turn carried image attachments; the agent emits
    /// them as OpenAI `image_url` content parts on that one turn. Pearl
    /// th-25ce5c.
    pub fn with_user_images(mut self, images: Vec<crate::conversation::ImageContent>) -> Self {
        self.next_user_images = images;
        self
    }

    /// Pre-seed the agent's conversation with prior turns. Pushed
    /// after the system prompt and before the current user message
    /// on each `run` / `run_with_channel`. Each `Message` should
    /// have role `User` or `Assistant` — anything else is silently
    /// dropped during the push.
    pub fn with_prior_messages(mut self, messages: Vec<Message>) -> Self {
        self.prior_messages = messages;
        self
    }

    /// Wire an injection channel for the agent's mailbox. Messages drained from
    /// this channel are pushed onto the conversation as user-turns at the top
    /// of each iteration, so the lead can steer/chat with a running teammate.
    pub fn with_chat_rx(mut self, rx: Arc<tokio::sync::Mutex<UnboundedReceiver<InjectedMessage>>>) -> Self {
        self.chat_rx = Some(rx);
        self
    }

    pub fn with_max_iterations(mut self, max: u32) -> Self {
        self.max_iterations = max;
        self
    }

    /// Append a "no final response until tests pass" rule to the
    /// system prompt. Stopgap for the full VERIFY phase (the
    /// architectural fix). Targets the failure mode surfaced by the
    /// 2026-05-29 coach matrix: deepseek/kimi/claude all stopped at
    /// 2-3 iterations with partial solutions (11/16, 18/20, 8/10)
    /// because the model decided it was done — not because the
    /// iteration cap was hit. glm-5.1 won by iterating 16 times
    /// naturally on the same task. The driver-side coach probe
    /// ("did you run the tests?") fires too late — the agent has
    /// already emitted Completed by the time the driver gets a turn.
    /// This rule applies the same intent INSIDE the agent loop where
    /// it actually fires.
    ///
    /// Opt-in so general `th code` sessions stay snappy (default off);
    /// bench dispatch turns it on. Idempotent: calling twice still
    /// appends only one copy of the rule.
    #[must_use]
    pub fn with_verify_tests_before_done(mut self, enabled: bool) -> Self {
        if !enabled || self.system_prompt.contains(VERIFY_TESTS_RULE_ANCHOR) {
            return self;
        }
        if !self.system_prompt.is_empty() && !self.system_prompt.ends_with('\n') {
            self.system_prompt.push('\n');
        }
        self.system_prompt.push('\n');
        self.system_prompt.push_str(VERIFY_TESTS_RULE);
        self
    }

    pub fn with_parallel_tools(mut self, enabled: bool) -> Self {
        self.parallel_tools = enabled;
        self
    }

    pub fn with_checkpoint_strategy(mut self, strategy: CheckpointStrategy) -> Self {
        self.checkpoint_strategy = strategy;
        self
    }

    pub fn with_compaction_strategy(mut self, strategy: CompactionStrategy) -> Self {
        self.compaction_strategy = strategy;
        self
    }

    pub fn with_memory(mut self, memory: Arc<dyn Memory>) -> Self {
        self.memory = Some(memory);
        self
    }

    pub fn with_knowledge(mut self, knowledge: Arc<dyn KnowledgeBase>) -> Self {
        self.knowledge = Some(knowledge);
        self
    }

    pub fn with_budget(mut self, budget: CostBudget) -> Self {
        self.budget = Some(budget);
        self
    }

    pub fn with_human_channel(mut self, tx: UnboundedSender<HumanRequest>, rx: Arc<tokio::sync::Mutex<UnboundedReceiver<HumanResponse>>>) -> Self {
        self.human_tx = Some(tx);
        self.human_rx = Some(rx);
        self
    }
}

/// Events emitted during agent execution.
#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type")]
pub enum AgentEvent {
    Started {
        agent_id: String,
    },
    LlmRequest {
        iteration: u32,
        message_count: usize,
    },
    LlmResponse {
        iteration: u32,
        content_preview: String,
        tool_call_count: usize,
    },
    ToolCallStart {
        iteration: u32,
        tool_name: String,
        /// Serialized JSON arguments the agent passed to the tool.
        /// Default = empty string so older runner builds that
        /// don't populate this still deserialize cleanly.
        #[serde(default)]
        arguments: String,
    },
    ToolCallComplete {
        iteration: u32,
        tool_name: String,
        is_error: bool,
        /// First ~500 chars of the tool's output (truncated to
        /// keep stdout-event sizes bounded). Default empty for
        /// older runners.
        #[serde(default)]
        result: String,
        /// Wall-clock duration of the tool call. Default 0 for
        /// older runners.
        #[serde(default)]
        duration_ms: u64,
    },
    CheckpointSaved {
        checkpoint_id: String,
        iteration: u32,
    },
    Completed {
        agent_id: String,
        iterations: u32,
        /// Accumulated cost in USD across every LLM call in this agent
        /// run. Defaults to 0 when deserializing older runner output
        /// that didn't carry the field.
        #[serde(default)]
        cost_usd: f64,
        /// Accumulated prompt tokens across every LLM call in this
        /// agent run. Used by downstream `[METRICS]` emitters as a
        /// fallback when the gateway/local pricing can't produce a
        /// useful cost_usd. Defaults to 0 for back-compat with
        /// older runner builds. Pearl th-eff0d0.
        #[serde(default)]
        prompt_tokens: u64,
        /// Accumulated completion tokens across every LLM call in
        /// this agent run. Same back-compat rules as `prompt_tokens`.
        #[serde(default)]
        completion_tokens: u64,
        /// Accumulated prompt-cache hits (subset of `prompt_tokens`).
        /// Sourced from `usage.prompt_tokens_details.cached_tokens` on
        /// each LLM response — non-zero only when the upstream supports
        /// Anthropic prompt caching AND the LiteLLM gateway has
        /// `cache_control_injection_points` configured. Lets a host
        /// surface a session's cache-hit ratio. Default 0 for older
        /// consumer builds.
        #[serde(default)]
        cached_tokens: u64,
    },
    /// Emitted by a multi-phase orchestrator each time it enters a new
    /// phase. The engine itself never emits this — it's a hook for
    /// consumers that drive the agent through their own phased loop and
    /// want a structured progress signal.
    ///
    /// Clients listen for this to update a status bar (phase name +
    /// routing alias + resolved upstream model). Serialization-compatible
    /// with consumers that don't emit it: clients just skip unknown
    /// `AgentEvent` variants.
    PhaseStart {
        /// Caller-defined phase name. Conventionally uppercase so it
        /// doubles as a display label.
        phase: String,
        /// Routing alias the phase dispatches through. Already
        /// resolved from the `Activity` slot so callers don't need
        /// access to the `ProviderRegistry`.
        alias: String,
        /// The concrete upstream model serving this phase, if known.
        /// `None` when no response has arrived yet (this phase's first
        /// turn) — clients display just the alias until one does.
        #[serde(default, skip_serializing_if = "Option::is_none")]
        upstream: Option<String>,
        /// 1-indexed iteration of the caller's outer loop. Lets a
        /// client show "iteration 3 of 5" style progress.
        iteration: u32,
    },
    MaxIterationsReached {
        agent_id: String,
        max: u32,
    },
    BudgetExceeded {
        spent_usd: f64,
        limit_usd: f64,
    },
    HumanInputRequired {
        request: HumanRequest,
    },
    HumanInputReceived {
        response: HumanResponse,
    },
    Error {
        message: String,
    },
    TokenDelta {
        content: String,
    },
    /// A streamed *reasoning* token from a reasoning-model's separate thinking
    /// channel (`reasoning_content`/`reasoning` deltas — Kimi, DeepSeek R1,
    /// gpt-oss/harmony, MiniMax). Kept distinct from [`TokenDelta`] so consumers
    /// can surface it as live "thinking" without it bleeding into the answer;
    /// the accumulator already drops reasoning from the final response content.
    /// Back-compat: older consumers skip this unknown variant, so they simply
    /// stop showing reasoning (rather than showing it as answer text).
    ReasoningDelta {
        content: String,
    },
    /// A streamed token of the fast-model *preamble* — a short, present-tense
    /// user-facing sentence describing what the agent is about to do, generated
    /// in parallel with the main model's first turn to cover its time-to-first-
    /// token (see [`AgentConfig::preamble`]). Distinct from [`TokenDelta`] so
    /// consumers render it as an *ephemeral* status line that the real answer
    /// replaces — never as permanent chat content. Back-compat: older consumers
    /// skip this unknown variant and simply don't show a preamble. Pearl th-9a5794.
    PreambleDelta {
        content: String,
    },
    StreamingComplete,
    DelegationStarted {
        parent_id: String,
        child_id: String,
        task: String,
    },
    DelegationCompleted {
        parent_id: String,
        child_id: String,
        success: bool,
    },
    /// An operator is exposing a guest port to the host.
    PortForwardActive {
        guest_port: u16,
        host_port: u16,
    },
    /// The gateway resolved the configured model alias to a concrete
    /// upstream model. Emitted once per agent session when the
    /// upstream differs from the alias (so TUIs can render
    /// `smooth-coding → qwen3-coder-flash`) and again only if the
    /// upstream changes mid-run. When alias == upstream, this event
    /// is suppressed to avoid clutter. Pearl th-a10c2d.
    ///
    /// Old runners that don't emit this event don't break anything —
    /// the TUI's `_ => {}` arm just ignores it for new clients
    /// connected to old servers, and old clients silently drop
    /// unknown variants on the deserialize side.
    ModelResolved {
        /// The alias the agent was configured with (e.g.
        /// `smooth-coding`). When the user pointed at a concrete
        /// model directly, this is just that model name.
        alias: String,
        /// The concrete upstream the gateway routed to (e.g.
        /// `qwen3-coder-flash`).
        upstream: String,
    },
    /// SEP: a new agent turn (one `run`) is starting. Emitted only when an
    /// extension host is attached; also fanned out to subscribed extensions as
    /// the `turn_start` event. Additive — clients skip unknown variants.
    TurnStart {
        agent_id: String,
    },
    /// SEP: the agent turn finished.
    TurnEnd {
        agent_id: String,
        iterations: u32,
    },
    /// SEP: an incremental update to the in-progress assistant message.
    /// Reserved for the streaming/observe wiring in a later phase; defined now
    /// so clients can round-trip it.
    MessageUpdate {
        iteration: u32,
        content_preview: String,
    },
    /// SEP: the assistant produced its final message this turn (maps to the
    /// `message_end` extension event).
    MessageEnd {
        iteration: u32,
        content_preview: String,
    },
    /// SEP: progress for an in-flight tool call (an extension `tool/update`, or
    /// a native tool's progress channel — benefits native tools too).
    ToolCallUpdate {
        iteration: u32,
        tool_name: String,
        message: String,
    },
}

/// SEP `tool_call` plan: the (possibly arg-modified) calls to execute plus the
/// set of vetoed call ids → reason. Built only when an extension host is
/// attached (see [`Agent::sep_tool_call_plan`]).
struct SepToolPlan {
    calls: Vec<crate::tool::ToolCall>,
    blocks: std::collections::HashMap<String, String>,
}

/// The lowercase wire name for a role in the SEP `context` hook's `{role, content}`
/// message shape.
fn role_wire_name(role: &Role) -> &'static str {
    match role {
        Role::System => "system",
        Role::User => "user",
        Role::Assistant => "assistant",
        Role::Tool => "tool",
    }
}

/// Rebuild a [`Message`] from a `context`-hook `{role, content}` wire object,
/// filling the engine-owned `id`/`timestamp`. Missing/blank `content` is fine
/// (an empty message); a missing `role` or a non-object entry is dropped. Any
/// role outside system/user/assistant collapses to user.
fn wire_message_to_message(v: &serde_json::Value) -> Option<Message> {
    let role = v.get("role").and_then(serde_json::Value::as_str)?;
    let content = v.get("content").and_then(serde_json::Value::as_str).unwrap_or_default();
    Some(match role {
        "system" => Message::system(content),
        "assistant" => Message::assistant(content),
        _ => Message::user(content),
    })
}

/// Configuration for a sub-agent spawned via delegation.
#[derive(Debug, Clone)]
pub struct SubAgentConfig {
    /// System prompt for the sub-agent.
    pub system_prompt: String,
    /// Maximum iterations for the sub-agent's run loop.
    pub max_iterations: u32,
    /// Whether to clone the parent's tools into the sub-agent.
    pub inherit_tools: bool,
}

impl Default for SubAgentConfig {
    fn default() -> Self {
        Self {
            system_prompt: "You are a sub-agent.".into(),
            max_iterations: 10,
            inherit_tools: true,
        }
    }
}

/// Handle to a delegated sub-agent task running in a background tokio task.
pub struct DelegationHandle {
    /// Unique ID of the sub-agent.
    pub agent_id: String,
    /// The task description given to the sub-agent.
    pub task: String,
    join_handle: tokio::task::JoinHandle<anyhow::Result<Conversation>>,
}

impl DelegationHandle {
    /// Wait for the sub-agent to finish and return its conversation.
    ///
    /// # Errors
    /// Returns error if the sub-agent panicked or returned an error.
    pub async fn wait(self) -> anyhow::Result<Conversation> {
        self.join_handle.await.map_err(|e| anyhow::anyhow!("sub-agent task panicked: {e}"))?
    }

    /// Cancel the sub-agent task.
    pub fn cancel(self) {
        self.join_handle.abort();
    }

    /// Check whether the sub-agent task has finished (completed, failed, or cancelled).
    pub fn is_finished(&self) -> bool {
        self.join_handle.is_finished()
    }
}

/// Built-in tool that delegates a task to a sub-agent.
///
/// When called with `{"task": "..."}`, spawns a sub-agent, waits for it
/// to complete, and returns the last assistant message as the tool result.
pub struct DelegationTool {
    agent: Arc<Agent>,
}

impl DelegationTool {
    /// Create a new `DelegationTool` backed by the given parent agent.
    pub fn new(agent: Arc<Agent>) -> Self {
        Self { agent }
    }
}

#[async_trait]
impl Tool for DelegationTool {
    fn schema(&self) -> ToolSchema {
        ToolSchema {
            name: "delegate".into(),
            description: "Delegate a task to a sub-agent that will work on it independently and return the result.".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": {
                    "task": {
                        "type": "string",
                        "description": "The task to delegate to the sub-agent"
                    }
                },
                "required": ["task"]
            }),
        }
    }

    async fn execute(&self, arguments: serde_json::Value) -> anyhow::Result<String> {
        let task = arguments
            .get("task")
            .and_then(|v| v.as_str())
            .ok_or_else(|| anyhow::anyhow!("missing required 'task' parameter"))?
            .to_string();

        let handle = self.agent.spawn_sub_agent(task, &SubAgentConfig::default());
        let conversation = handle.wait().await?;

        // Return the last assistant message as the result
        let last_assistant = conversation
            .last_assistant_content()
            .unwrap_or("Sub-agent completed with no response.")
            .to_string();

        Ok(last_assistant)
    }
}

/// An AI agent that runs an observe → think → act loop.
pub struct Agent {
    pub id: String,
    config: AgentConfig,
    tools: ToolRegistry,
    checkpoint_store: Option<Arc<dyn CheckpointStore>>,
    event_handler: Option<Box<dyn Fn(AgentEvent) + Send + Sync>>,
    reactive_compaction: std::sync::Mutex<ReactiveCompaction>,
    pub cost_tracker: Arc<Mutex<CostTracker>>,
    /// Last upstream model the gateway resolved this agent's alias
    /// to. Used to decide whether to emit `AgentEvent::ModelResolved`:
    /// emit on the first non-empty resolution that differs from the
    /// configured alias, then again only when the upstream changes.
    /// `Mutex<Option<String>>` so both `run` and `run_streaming`
    /// (which take `&self`) can update it. Pearl th-a10c2d.
    last_resolved_model: std::sync::Mutex<Option<String>>,
    /// Optional override for the LLM call surface. When `None` (production
    /// default), each run builds a real [`LlmClient`] from `config.llm`. Tests
    /// inject a [`MockLlmClient`](crate::llm_provider::MockLlmClient) here to
    /// drive the loop deterministically without a live model.
    llm_provider: Option<Arc<dyn LlmProvider>>,
    /// Optional SEP extension host. When `None` (the default) the agent loop is
    /// exactly as it was before extensions existed. When set, the agent runs
    /// the `tool_call` hook chain before executing tool calls and fans turn
    /// events out to subscribed extensions.
    extension_host: Option<Arc<crate::extension::ExtensionHost>>,
    /// Posture for the permission gate installed by [`with_extension_host`](Self::with_extension_host).
    /// Defaults to [`AutoMode::from_env`] (reads `SMOOTH_AUTO_MODE`, default
    /// `Ask`). Override via [`with_permission_mode`](Self::with_permission_mode)
    /// *before* attaching the host. Pearl th-d32ce6.
    permission_mode: crate::permission::AutoMode,
    /// Optional consumer-supplied deny policy (pearl th-deny-policy). `None` (the
    /// default) leaves enforcement byte-identical. When set, it is attached to
    /// the [`PermissionHook`](crate::permission::PermissionHook) installed by
    /// [`with_extension_host`](Self::with_extension_host) and evaluated first —
    /// a policy match is a circuit-breaker. Set via
    /// [`with_deny_policy`](Self::with_deny_policy) *before* attaching the host.
    deny_policy: Option<Arc<crate::deny_policy::DenyPolicy>>,
}

impl Agent {
    pub fn new(config: AgentConfig, tools: ToolRegistry) -> Self {
        Self {
            id: uuid::Uuid::new_v4().to_string(),
            config,
            tools,
            checkpoint_store: None,
            event_handler: None,
            reactive_compaction: std::sync::Mutex::new(ReactiveCompaction::new()),
            cost_tracker: Arc::new(Mutex::new(CostTracker::default())),
            last_resolved_model: std::sync::Mutex::new(None),
            llm_provider: None,
            extension_host: None,
            permission_mode: crate::permission::AutoMode::from_env(),
            deny_policy: None,
        }
    }

    /// Attach a consumer-supplied [`DenyPolicy`](crate::deny_policy::DenyPolicy)
    /// (pearl th-deny-policy). Purely additive: with no policy (the default)
    /// behavior is unchanged. Call this **before**
    /// [`with_extension_host`](Self::with_extension_host), which reads it when it
    /// installs the permission gate. A policy match is a circuit-breaker — it
    /// wins over stored grants and every [`AutoMode`](crate::permission::AutoMode),
    /// `Bypass` included.
    #[must_use]
    pub fn with_deny_policy(mut self, policy: Arc<crate::deny_policy::DenyPolicy>) -> Self {
        self.deny_policy = Some(policy);
        self
    }

    /// Set the [`AutoMode`](crate::permission::AutoMode) posture for the
    /// permission gate. Call this **before** [`with_extension_host`](Self::with_extension_host),
    /// which reads the mode when it installs the hook. Consumers with an
    /// interactive approver (or that trust their extensions) opt into
    /// [`AutoMode::Bypass`](crate::permission::AutoMode::Bypass) here — the hard
    /// circuit-breakers (credential paths, `rm -rf /`, pipe-to-shell, dangerous
    /// domains, env dumps) still fire in every mode. Pearl th-d32ce6.
    #[must_use]
    pub fn with_permission_mode(mut self, mode: crate::permission::AutoMode) -> Self {
        self.permission_mode = mode;
        self
    }

    /// Attach a SEP [`ExtensionHost`](crate::extension::ExtensionHost). Purely
    /// additive: with no host attached (the default) the agent loop is
    /// unchanged. With a host, the agent runs the fail-closed `tool_call` hook
    /// chain before executing tool calls (extensions may veto), emits/fans out
    /// turn events, AND registers every extension tool into the agent's
    /// [`ToolRegistry`](crate::ToolRegistry) as an ordinary tool named
    /// `<extension>.<tool>`. Because they are ordinary registry tools, they are
    /// visible to the LLM via `schemas()`, dispatched via `execute()`, and
    /// filtered by the exact same `retain()` mechanism a server uses to enforce
    /// its per-agent `enabled_tools` allow-list — no special casing anywhere.
    #[must_use]
    pub fn with_extension_host(mut self, host: Arc<crate::extension::ExtensionHost>) -> Self {
        for tool in host.tools() {
            self.tools.register_arc(tool);
        }
        for tool in host.deferred_tools() {
            self.tools.register_deferred_arc(tool);
        }
        // Gate every tool call on this registry — extension-contributed tools
        // in particular had no permission check (pearl th-d32ce6). The
        // classifier returns allow / ask / deny; `deny` blocks. An `ask` is
        // routed to a human when a human channel is wired (via
        // `with_human_channel`, pearl th-6b3ab4) and fails closed otherwise.
        // Mode from `SMOOTH_AUTO_MODE` (default `Ask`). Added last so it runs
        // after any role-clearance `PermissionHook` already installed — a `deny`
        // from either blocks, so ordering only affects which reason surfaces first.
        let mut permission_hook = crate::permission::PermissionHook::new(self.permission_mode);
        // Attach the consumer deny policy (pearl th-deny-policy) if one was set —
        // evaluated first in `pre_call`, so a policy match is a circuit-breaker.
        if let Some(policy) = &self.deny_policy {
            permission_hook = permission_hook.with_deny_policy(policy.clone());
        }
        if let (Some(tx), Some(rx)) = (self.config.human_tx.clone(), self.config.human_rx.clone()) {
            // ponytail: 5-min approval window; consumer sets the channel, this is
            // the default wait before an unanswered ask fails closed.
            permission_hook = permission_hook.with_approver(tx, rx, std::time::Duration::from_secs(300));
        }
        // Wire the persistent allow-list (pearl th-22bfc1): approvals are
        // remembered across runs so an `Ask` matching a stored grant
        // auto-approves without re-prompting. Load stacks the user file
        // (`~/.smooth/wonk-allow.toml`) under the project file
        // (`<cwd>/.smooth/wonk-allow.toml`, project wins); `ApprovedAlways`
        // persists new grants to the user file. A missing home dir (rare CI)
        // just leaves the allow-list off — every `Ask` still prompts.
        if let Some(user_path) = crate::permission_grants::user_grants_path() {
            let project_path = std::env::current_dir().ok().map(|cwd| crate::permission_grants::project_grants_path(&cwd));
            match crate::permission_grants::PermissionGrants::load_layered(Some(&user_path), project_path.as_deref()) {
                Ok(grants) => {
                    permission_hook = permission_hook.with_grants(crate::permission_grants::SharedGrants::new(grants), user_path);
                }
                // A malformed allow-list must fail loud, not silently grant nothing.
                Err(e) => tracing::warn!("permission allow-list disabled — {e}"),
            }
        }
        self.tools.add_hook(permission_hook);
        // Then scan the calls that clear the permission gate for secrets +
        // prompt injection (pearl th-5f7227). Extension arguments went to the
        // subprocess unscanned and results came back verbatim; this Narc-style
        // hook blocks arguments carrying an exfiltration payload and surveils
        // (detect + alert) everything else. Added after the permission gate so
        // allow/ask/deny is decided first. On results it redacts leaked secrets
        // in place (mutable `post_call` seam, pearl th-10eb50).
        self.tools.add_hook(crate::narc::NarcHook::new());
        self.extension_host = Some(host);
        self
    }

    /// Inject a custom [`LlmProvider`] (e.g. a
    /// [`MockLlmClient`](crate::llm_provider::MockLlmClient) in tests). When set,
    /// `run` / `run_with_channel` use it instead of building an [`LlmClient`]
    /// from `config.llm`.
    #[must_use]
    pub fn with_llm_provider(mut self, provider: Arc<dyn LlmProvider>) -> Self {
        self.llm_provider = Some(provider);
        self
    }

    pub fn with_checkpoint_store(mut self, store: Arc<dyn CheckpointStore>) -> Self {
        self.checkpoint_store = Some(store);
        self
    }

    pub fn with_event_handler(mut self, handler: impl Fn(AgentEvent) + Send + Sync + 'static) -> Self {
        self.event_handler = Some(Box::new(handler));
        self
    }

    /// Spawn a sub-agent to work on the given task in a background tokio task.
    ///
    /// The sub-agent inherits the parent's `LlmConfig`. If `sub_config.inherit_tools`
    /// is true, the parent's tool registry is cloned (tool `Arc`s are shared, hooks are not).
    pub fn spawn_sub_agent(self: &Arc<Self>, task: String, sub_config: &SubAgentConfig) -> DelegationHandle {
        let tools = if sub_config.inherit_tools {
            self.tools.clone_tools()
        } else {
            ToolRegistry::new()
        };

        let child_config = AgentConfig::new(format!("{}-sub", self.config.name), &sub_config.system_prompt, self.config.llm.clone())
            .with_max_iterations(sub_config.max_iterations);

        let child = Self::new(child_config, tools);
        let child_id = child.id.clone();

        self.emit(AgentEvent::DelegationStarted {
            parent_id: self.id.clone(),
            child_id: child_id.clone(),
            task: task.clone(),
        });

        let task_for_spawn = task.clone();
        let join_handle = tokio::spawn(async move { child.run(&task_for_spawn).await });

        DelegationHandle {
            agent_id: child_id,
            task,
            join_handle,
        }
    }

    /// Resume from the latest checkpoint, or start fresh.
    ///
    /// # Errors
    /// Returns error if checkpoint loading fails.
    pub fn resume_or_new(&self) -> anyhow::Result<Conversation> {
        if let Some(store) = &self.checkpoint_store {
            if let Some(checkpoint) = store.load_latest(&self.id)? {
                tracing::info!(agent_id = %self.id, checkpoint_id = %checkpoint.id, iteration = checkpoint.iteration, "resuming from checkpoint");
                return Ok(checkpoint.conversation);
            }
        }
        Ok(Conversation::new(self.config.max_context_tokens).with_system_prompt(&self.config.system_prompt))
    }

    /// Run the agent loop with a user message.
    ///
    /// # Errors
    /// Returns error if the LLM call or tool execution fails fatally.
    #[allow(clippy::too_many_lines)]
    pub async fn run(&self, user_message: impl Into<String>) -> anyhow::Result<Conversation> {
        let mut conversation = self.resume_or_new()?;
        self.sep_before_agent_start(&mut conversation).await;
        let user_msg: String = user_message.into();

        // Pre-seed prior session turns (pearl th-422b93) before
        // memory/knowledge injection. Only User/Assistant roles are
        // preserved — System prompts already came from
        // `resume_or_new`, and Tool messages would dangle without
        // matching tool_calls.
        for msg in &self.config.prior_messages {
            if matches!(msg.role, Role::User | Role::Assistant) {
                conversation.push(msg.clone());
            }
        }

        // Inject memory/knowledge context before the user message
        let context_messages = self.build_context_messages(&user_msg);
        for msg in context_messages {
            conversation.push(msg);
        }

        // Attach any pending image content to this turn's user message
        // (a multimodal turn). Consumed on this turn only. Pearl th-25ce5c.
        let user_message = if self.config.next_user_images.is_empty() {
            Message::user(user_msg)
        } else {
            Message::user_with_images(user_msg, self.config.next_user_images.clone())
        };
        conversation.push(user_message);

        self.emit(AgentEvent::Started { agent_id: self.id.clone() });
        if let Some(host) = &self.extension_host {
            host.dispatch_event(crate::extension::events::TURN_START, serde_json::json!({ "agent_id": self.id }));
            self.emit(AgentEvent::TurnStart { agent_id: self.id.clone() });
        }

        let llm: Arc<dyn LlmProvider> = match &self.llm_provider {
            Some(provider) => Arc::clone(provider),
            None => Arc::new(LlmClient::new(self.config.llm.clone()).with_model_ceiling(self.config.model_max_output)),
        };

        for iteration in 1..=self.config.max_iterations {
            // Recompute schemas every iteration so tools promoted
            // by `tool_search` mid-run land in the LLM's tool list
            // on the next turn (pearl th-cfa1fb). Cheap: schemas()
            // walks the registry's tools map and clones each
            // schema, no LLM-side I/O.
            let tool_schemas = self.tools.schemas();

            // Drain mailbox injections (see `drain_injected_messages` doc).
            self.drain_injected_messages(&mut conversation);

            // On the final iteration, surface the max-steps reminder so the
            // model can write a clean wrap-up turn instead of being cut off
            // mid-tool-chain. Only inject once — if max_iterations == 1
            // the reminder still lands before the single LLM call.
            if iteration == self.config.max_iterations {
                conversation.push(Message::system(MAX_STEPS_REMINDER));
            }

            // Compact if approaching context limit
            if conversation.needs_compaction() {
                let result = conversation.compact(&self.config.compaction_strategy, None);
                tracing::info!(
                    messages_removed = result.messages_removed,
                    tokens_before = result.tokens_before,
                    tokens_after = result.tokens_after,
                    "compacted conversation"
                );
            }

            // Observe: get context window
            let context = conversation.context_window();
            // SEP `context` hook may replace the whole message array before the
            // LLM sees it; `None` keeps the borrowed, zero-copy path (unhooked).
            let context_rewrite = self.sep_context_rewrite(&context).await;
            let context_refs: Vec<&Message> = match &context_rewrite {
                Some(msgs) => msgs.iter().collect(),
                None => context.to_vec(),
            };

            self.emit(AgentEvent::LlmRequest {
                iteration,
                message_count: context_refs.len(),
            });

            // Think: call LLM (with reactive compaction on context-length errors)
            let response = match llm.chat(&context_refs, &tool_schemas).await {
                Ok(resp) => resp,
                Err(e) => {
                    let err_msg = e.to_string();
                    if err_msg.contains("prompt_too_long") || err_msg.contains("context_length_exceeded") {
                        // Check circuit breaker before attempting reactive compaction
                        {
                            let rc = self.reactive_compaction.lock().expect("lock reactive_compaction");
                            if rc.is_circuit_open() {
                                return Err(anyhow::anyhow!(
                                    "reactive compaction circuit breaker open after {} consecutive failures: {err_msg}",
                                    rc.stats().consecutive_failures
                                ));
                            }
                        }

                        // Compact the conversation reactively
                        let result = conversation.compact(&self.config.compaction_strategy, None);
                        tracing::warn!(
                            messages_removed = result.messages_removed,
                            tokens_before = result.tokens_before,
                            tokens_after = result.tokens_after,
                            "reactive compaction triggered by context length error"
                        );

                        // Retry with compacted context
                        let retry_context = conversation.context_window();
                        let retry_refs: Vec<&Message> = retry_context.into_iter().collect();
                        match llm.chat(&retry_refs, &tool_schemas).await {
                            Ok(resp) => {
                                self.reactive_compaction.lock().expect("lock reactive_compaction").record_success();
                                resp
                            }
                            Err(retry_err) => {
                                self.reactive_compaction.lock().expect("lock reactive_compaction").record_failure();
                                return Err(retry_err);
                            }
                        }
                    } else {
                        return Err(e);
                    }
                }
            };

            let content_preview = response.content.chars().take(100).collect::<String>();
            self.emit(AgentEvent::LlmResponse {
                iteration,
                content_preview,
                tool_call_count: response.tool_calls.len(),
            });

            // Surface the resolved upstream model once per session
            // (and again only if it changes). Pearl th-a10c2d.
            if let Some(event) = self.model_resolution_event(&response) {
                self.emit(event);
            }

            // Record cost and check budget
            if self.record_cost_and_check_budget(&response) {
                return Ok(conversation);
            }

            // If LLM returned content, add it as assistant message
            if !response.content.is_empty() || !response.tool_calls.is_empty() || response.reasoning_content.is_some() {
                let mut msg = Message::assistant(&response.content);
                msg.tool_calls.clone_from(&response.tool_calls);
                // Pearl th-eae0f8: preserve reasoning so the next
                // turn's wire request includes it. LiteLLM thinking-
                // mode upstreams 400 us without this.
                msg.reasoning_content.clone_from(&response.reasoning_content);
                conversation.push(msg);
            }

            // Maybe checkpoint after LLM response
            self.maybe_checkpoint(&conversation, iteration, CheckpointEvent::LlmResponse);

            // Act: execute tool calls
            if response.tool_calls.is_empty() {
                // No tool calls = agent is done thinking
                let (cost, prompt_tokens, completion_tokens, cached_tokens) = {
                    let tracker = self.cost_tracker.lock().expect("lock cost_tracker");
                    (
                        tracker.total_cost_usd,
                        tracker.total_prompt_tokens,
                        tracker.total_completion_tokens,
                        tracker.total_cached_tokens,
                    )
                };
                self.emit(AgentEvent::Completed {
                    agent_id: self.id.clone(),
                    iterations: iteration,
                    cost_usd: cost,
                    prompt_tokens,
                    completion_tokens,
                    cached_tokens,
                });
                if self.extension_host.is_some() {
                    let preview: String = response.content.chars().take(100).collect();
                    self.sep_dispatch(
                        crate::extension::events::MESSAGE_END,
                        serde_json::json!({ "iteration": iteration, "content": response.content }),
                    );
                    self.sep_dispatch(
                        crate::extension::events::TURN_END,
                        serde_json::json!({ "agent_id": self.id, "iterations": iteration }),
                    );
                    self.emit(AgentEvent::MessageEnd {
                        iteration,
                        content_preview: preview,
                    });
                    self.emit(AgentEvent::TurnEnd {
                        agent_id: self.id.clone(),
                        iterations: iteration,
                    });
                }
                return Ok(conversation);
            }

            // SEP: fail-closed `tool_call` hook (Block vetoes, Modify rewrites
            // args) BEFORE execution; the registry's own ToolHooks still veto
            // inside `execute`/`execute_parallel` (clean layering). `None` when
            // no host is attached → unchanged, allocation-free path.
            let sep_plan = self.sep_tool_call_plan(&response.tool_calls).await;
            let calls = sep_plan.as_ref().map_or(response.tool_calls.as_slice(), |p| p.calls.as_slice());
            let blocks = sep_plan.as_ref().map(|p| &p.blocks);
            self.sep_run_tool_calls(calls, blocks, iteration, &mut conversation, None).await;
        }

        self.emit(AgentEvent::MaxIterationsReached {
            agent_id: self.id.clone(),
            max: self.config.max_iterations,
        });

        Ok(conversation)
    }

    /// Run the agent loop with streaming LLM responses, sending events through a channel.
    ///
    /// This is the streaming counterpart to `run()`. Instead of using the closure-based
    /// event handler, all events (including token deltas) are sent through the provided
    /// `mpsc::UnboundedSender`. This is designed for TUI consumption.
    ///
    /// # Errors
    /// Returns error if the LLM call or tool execution fails fatally.
    #[allow(clippy::too_many_lines)]
    pub async fn run_with_channel(&self, user_message: impl Into<String>, tx: tokio::sync::mpsc::UnboundedSender<AgentEvent>) -> anyhow::Result<Conversation> {
        let mut conversation = self.resume_or_new()?;
        self.sep_before_agent_start(&mut conversation).await;
        let user_msg: String = user_message.into();

        // Fire the parallel fast-model preamble (best-effort UX; no-op unless
        // `config.preamble` is set). Kicked as early as possible — before the
        // main model's first turn — so its one-sentence "what I'm about to do"
        // covers the reasoning model's time-to-first-token. Pearl th-9a5794.
        self.spawn_preamble(&user_msg, &tx);

        // Pre-seed prior session turns (pearl th-422b93) before
        // memory/knowledge injection. Only User/Assistant roles are
        // preserved — System prompts already came from
        // `resume_or_new`, and Tool messages would dangle without
        // matching tool_calls.
        for msg in &self.config.prior_messages {
            if matches!(msg.role, Role::User | Role::Assistant) {
                conversation.push(msg.clone());
            }
        }

        // Inject memory/knowledge context before the user message
        let context_messages = self.build_context_messages(&user_msg);
        for msg in context_messages {
            conversation.push(msg);
        }

        // Attach any pending image content to this turn's user message
        // (a multimodal turn). Consumed on this turn only. Pearl th-25ce5c.
        let user_message = if self.config.next_user_images.is_empty() {
            Message::user(user_msg)
        } else {
            Message::user_with_images(user_msg, self.config.next_user_images.clone())
        };
        conversation.push(user_message);

        let _ = tx.send(AgentEvent::Started { agent_id: self.id.clone() });
        if self.extension_host.is_some() {
            self.sep_dispatch(crate::extension::events::TURN_START, serde_json::json!({ "agent_id": self.id }));
            let _ = tx.send(AgentEvent::TurnStart { agent_id: self.id.clone() });
        }

        let llm: Arc<dyn LlmProvider> = match &self.llm_provider {
            Some(provider) => Arc::clone(provider),
            None => Arc::new(LlmClient::new(self.config.llm.clone()).with_model_ceiling(self.config.model_max_output)),
        };

        for iteration in 1..=self.config.max_iterations {
            // Recompute schemas every iteration so tools promoted
            // by `tool_search` mid-run become callable next turn
            // (pearl th-cfa1fb).
            let tool_schemas = self.tools.schemas();

            // Drain any out-of-band injected messages (mailbox) and push them as
            // user-turns. This is what makes the agent conversational mid-flight:
            // the lead, a direct-chat user, or an answer to an `ask_smooth` call
            // all arrive here.
            self.drain_injected_messages(&mut conversation);

            // Final iteration: surface the max-steps reminder so the model
            // can wrap up cleanly instead of being cut off mid-tool-chain.
            if iteration == self.config.max_iterations {
                conversation.push(Message::system(MAX_STEPS_REMINDER));
            }

            // Compact if approaching context limit
            if conversation.needs_compaction() {
                let result = conversation.compact(&self.config.compaction_strategy, None);
                tracing::info!(
                    messages_removed = result.messages_removed,
                    tokens_before = result.tokens_before,
                    tokens_after = result.tokens_after,
                    "compacted conversation"
                );
            }

            let context = conversation.context_window();
            // SEP `context` hook may replace the whole message array before the
            // LLM sees it; `None` keeps the borrowed, zero-copy path (unhooked).
            let context_rewrite = self.sep_context_rewrite(&context).await;
            let context_refs: Vec<&Message> = match &context_rewrite {
                Some(msgs) => msgs.iter().collect(),
                None => context.to_vec(),
            };

            let _ = tx.send(AgentEvent::LlmRequest {
                iteration,
                message_count: context_refs.len(),
            });

            // Stream the LLM response (with reactive compaction on context-length errors)
            let mut stream = match llm.chat_stream(&context_refs, &tool_schemas).await {
                Ok(s) => s,
                Err(e) => {
                    let err_msg = e.to_string();
                    if err_msg.contains("prompt_too_long") || err_msg.contains("context_length_exceeded") {
                        {
                            let rc = self.reactive_compaction.lock().expect("lock reactive_compaction");
                            if rc.is_circuit_open() {
                                return Err(anyhow::anyhow!(
                                    "reactive compaction circuit breaker open after {} consecutive failures: {err_msg}",
                                    rc.stats().consecutive_failures
                                ));
                            }
                        }

                        let result = conversation.compact(&self.config.compaction_strategy, None);
                        tracing::warn!(
                            messages_removed = result.messages_removed,
                            tokens_before = result.tokens_before,
                            tokens_after = result.tokens_after,
                            "reactive compaction triggered by context length error (streaming)"
                        );

                        let retry_context = conversation.context_window();
                        let retry_refs: Vec<&Message> = retry_context.into_iter().collect();
                        match llm.chat_stream(&retry_refs, &tool_schemas).await {
                            Ok(s) => {
                                self.reactive_compaction.lock().expect("lock reactive_compaction").record_success();
                                s
                            }
                            Err(retry_err) => {
                                self.reactive_compaction.lock().expect("lock reactive_compaction").record_failure();
                                return Err(retry_err);
                            }
                        }
                    } else {
                        return Err(e);
                    }
                }
            };

            // Forward token deltas through the channel while accumulating
            let (accumulator_tx, accumulator_rx) = tokio::sync::mpsc::channel::<anyhow::Result<StreamEvent>>(256);

            // Hard per-iteration wall clock — if a single LLM turn takes longer than
            // this, abort and move on. Guards against provider streams that go into
            // TCP CLOSE_WAIT without producing EOF (observed on some OpenAI-compat
            // proxies). Applies to BOTH the tap loop and accumulator.
            const ITERATION_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(600);
            // Per-item idle timeout inside the tap loop — same guard, shorter scope.
            // Reasoning models (MiniMax-M1, DeepSeek R1) can pause 60-120s between
            // chunks during deep thinking. 120s idle is generous but safe.
            const ITEM_IDLE_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(120);

            let tap_tx = tx.clone();
            let tap_loop = async {
                loop {
                    let event_result = match tokio::time::timeout(ITEM_IDLE_TIMEOUT, stream.next()).await {
                        Ok(Some(r)) => r,
                        Ok(None) => break, // stream ended cleanly
                        Err(_) => {
                            // Idle timeout — surface as an error so accumulator fails fast.
                            let _ = accumulator_tx
                                .send(Err(anyhow::anyhow!("LLM stream idle timeout: no event for {ITEM_IDLE_TIMEOUT:?}")))
                                .await;
                            return;
                        }
                    };
                    match &event_result {
                        Ok(StreamEvent::Delta { content }) => {
                            let _ = tap_tx.send(AgentEvent::TokenDelta { content: content.clone() });
                        }
                        Ok(StreamEvent::Reasoning { content }) => {
                            // Surface reasoning on its OWN event so consumers can show it
                            // as live "thinking" without it bleeding into the answer
                            // (Kimi K2.5, DeepSeek R1, gpt-oss/harmony, etc.). The
                            // accumulator still drops reasoning from the final response
                            // content, so the answer stays clean either way.
                            let _ = tap_tx.send(AgentEvent::ReasoningDelta { content: content.clone() });
                        }
                        Ok(StreamEvent::Done { .. }) => {
                            let _ = tap_tx.send(AgentEvent::StreamingComplete);
                        }
                        _ => {}
                    }
                    if accumulator_tx.send(event_result).await.is_err() {
                        break;
                    }
                }
                drop(accumulator_tx);
            };

            let rx_stream = tokio_stream::wrappers::ReceiverStream::new(accumulator_rx);
            let accumulate_fut = accumulate_stream_events(Box::pin(rx_stream));

            // Run tap and accumulate concurrently, under a hard wall-clock cap.
            let (_, accumulated) = match tokio::time::timeout(ITERATION_TIMEOUT, async {
                let (_, acc) = tokio::join!(tap_loop, accumulate_fut);
                acc
            })
            .await
            {
                Ok(result) => ((), result?),
                Err(_) => {
                    return Err(anyhow::anyhow!(
                        "LLM iteration timeout: no completion within {ITERATION_TIMEOUT:?} on iteration {iteration}"
                    ));
                }
            };
            let response = accumulated;

            let content_preview = response.content.chars().take(100).collect::<String>();
            let _ = tx.send(AgentEvent::LlmResponse {
                iteration,
                content_preview,
                tool_call_count: response.tool_calls.len(),
            });

            // Surface the resolved upstream model once per session
            // (and again only if it changes). Pearl th-a10c2d.
            if let Some(event) = self.model_resolution_event(&response) {
                let _ = tx.send(event);
            }

            // Record cost and check budget
            if self.record_cost_and_check_budget(&response) {
                return Ok(conversation);
            }

            if !response.content.is_empty() || !response.tool_calls.is_empty() || response.reasoning_content.is_some() {
                let mut msg = Message::assistant(&response.content);
                msg.tool_calls.clone_from(&response.tool_calls);
                // Pearl th-eae0f8: preserve reasoning so the next
                // turn's wire request includes it. LiteLLM thinking-
                // mode upstreams 400 us without this.
                msg.reasoning_content.clone_from(&response.reasoning_content);
                conversation.push(msg);
            }

            self.maybe_checkpoint(&conversation, iteration, CheckpointEvent::LlmResponse);

            if response.tool_calls.is_empty() {
                let (cost, prompt_tokens, completion_tokens, cached_tokens) = {
                    let tracker = self.cost_tracker.lock().expect("lock cost_tracker");
                    (
                        tracker.total_cost_usd,
                        tracker.total_prompt_tokens,
                        tracker.total_completion_tokens,
                        tracker.total_cached_tokens,
                    )
                };
                let _ = tx.send(AgentEvent::Completed {
                    agent_id: self.id.clone(),
                    iterations: iteration,
                    cost_usd: cost,
                    prompt_tokens,
                    completion_tokens,
                    cached_tokens,
                });
                if self.extension_host.is_some() {
                    let preview: String = response.content.chars().take(100).collect();
                    self.sep_dispatch(
                        crate::extension::events::MESSAGE_END,
                        serde_json::json!({ "iteration": iteration, "content": response.content }),
                    );
                    self.sep_dispatch(
                        crate::extension::events::TURN_END,
                        serde_json::json!({ "agent_id": self.id, "iterations": iteration }),
                    );
                    let _ = tx.send(AgentEvent::MessageEnd {
                        iteration,
                        content_preview: preview,
                    });
                    let _ = tx.send(AgentEvent::TurnEnd {
                        agent_id: self.id.clone(),
                        iterations: iteration,
                    });
                }
                return Ok(conversation);
            }

            // SEP tool_call hook (Block/Modify) + shared execution path — see
            // `sep_run_tool_calls`. `None` plan → unchanged, allocation-free.
            let sep_plan = self.sep_tool_call_plan(&response.tool_calls).await;
            let calls = sep_plan.as_ref().map_or(response.tool_calls.as_slice(), |p| p.calls.as_slice());
            let blocks = sep_plan.as_ref().map(|p| &p.blocks);
            self.sep_run_tool_calls(calls, blocks, iteration, &mut conversation, Some(&tx)).await;
        }

        let _ = tx.send(AgentEvent::MaxIterationsReached {
            agent_id: self.id.clone(),
            max: self.config.max_iterations,
        });

        Ok(conversation)
    }

    /// Build context injection messages from memory and knowledge based on the last user message.
    fn build_context_messages(&self, last_user_message: &str) -> Vec<Message> {
        use std::fmt::Write;
        let mut context_parts = Vec::new();

        if let Some(memory) = &self.config.memory {
            match memory.recall(last_user_message, 5) {
                Ok(entries) if !entries.is_empty() => {
                    let needs_freshness = entries.iter().any(|e| e.memory_type.needs_freshness_check());
                    let mut buf = String::from("[Recalled memories]\n");
                    if needs_freshness {
                        // D6: verify-before-recommend rule — a memory that names
                        // a function/file/flag is a claim about the past, not a
                        // fact about now. The agent should grep/read before
                        // surfacing it, especially for Project and Reference
                        // entries which are time-sensitive.
                        buf.push_str(
                            "Note: 'the memory says X exists' is not the same as 'X exists now'. \
                            Before recommending or acting on any function path, file, flag, or external \
                            pointer named below, verify it's current by reading the file or grepping the \
                            codebase. Project and Reference memories are time-sensitive; User and Feedback \
                            are durable.\n",
                        );
                    }
                    for entry in &entries {
                        let _ = writeln!(buf, "- ({:?}, relevance={:.2}): {}", entry.memory_type, entry.relevance, entry.content);
                    }
                    context_parts.push(buf);
                }
                Err(e) => {
                    tracing::warn!(error = %e, "failed to recall memories");
                }
                _ => {}
            }
        }

        if let Some(knowledge) = &self.config.knowledge {
            match knowledge.query(last_user_message, 3) {
                Ok(results) if !results.is_empty() => {
                    let mut buf = String::from("[Relevant knowledge]\n");
                    for result in &results {
                        let _ = writeln!(buf, "- (source={}, score={:.2}): {}", result.source, result.score, result.chunk);
                    }
                    context_parts.push(buf);
                }
                Err(e) => {
                    tracing::warn!(error = %e, "failed to query knowledge base");
                }
                _ => {}
            }
        }

        context_parts.into_iter().map(Message::system).collect()
    }

    fn maybe_checkpoint(&self, conversation: &Conversation, iteration: u32, event: CheckpointEvent) {
        if !self.config.checkpoint_strategy.should_checkpoint(iteration, event) {
            return;
        }

        if let Some(store) = &self.checkpoint_store {
            let checkpoint = Checkpoint::new(&self.id, conversation, iteration);
            let checkpoint_id = checkpoint.id.clone();
            match store.save(&checkpoint) {
                Ok(()) => {
                    self.emit(AgentEvent::CheckpointSaved { checkpoint_id, iteration });
                }
                Err(e) => {
                    tracing::warn!(error = %e, "failed to save checkpoint");
                }
            }
        }
    }

    /// Record cost for an LLM response and check budget. Returns `true` if budget was exceeded.
    ///
    /// Prefers the gateway's authoritative cost when present
    /// (LiteLLM's `x-litellm-response-cost` header). Falls back to
    /// the local `ModelPricing` table otherwise — which is only
    /// accurate for direct provider access; aliased routes through
    /// `smooth-coding` et al. won't price correctly locally.
    fn record_cost_and_check_budget(&self, response: &crate::llm::LlmResponse) -> bool {
        let model = &self.config.llm.model;

        {
            let mut tracker = self.cost_tracker.lock().expect("lock cost_tracker");
            if let Some(cost) = response.gateway_cost_usd {
                tracker.record_with_cost(model, &response.usage, cost);
            } else {
                let pricing = ModelPricing::for_model(model);
                tracker.record(model, &response.usage, &pricing);
            }

            if let Some(budget) = &self.config.budget {
                if let Err(exceeded) = tracker.check_budget(budget) {
                    self.emit(AgentEvent::BudgetExceeded {
                        spent_usd: exceeded.spent_usd,
                        limit_usd: exceeded.limit_usd.unwrap_or(0.0),
                    });
                    return true;
                }
            }
        }

        false
    }

    fn emit(&self, event: AgentEvent) {
        if let Some(handler) = &self.event_handler {
            handler(event);
        }
    }

    /// SEP: run the fail-closed `tool_call` hook chain over the pending calls.
    /// Returns `None` when no extension host is attached, so callers stay on the
    /// unchanged, allocation-free execution path. When a host is present, each
    /// call is either vetoed (id → reason in `blocks`) or passed through with its
    /// arguments possibly rewritten by a `Modify` outcome (in `calls`, same
    /// order as the input). The registry's own `ToolHook`s still veto inside
    /// `execute`/`execute_parallel` afterward — clean layering.
    async fn sep_tool_call_plan(&self, calls: &[crate::tool::ToolCall]) -> Option<SepToolPlan> {
        let host = self.extension_host.as_ref()?;
        let mut out = Vec::with_capacity(calls.len());
        let mut blocks = std::collections::HashMap::new();
        for call in calls {
            match host.run_tool_call_hook(&call.name, &call.arguments).await {
                crate::extension::FoldedHook::Blocked(reason) => {
                    blocks.insert(call.id.clone(), reason);
                    out.push(call.clone());
                }
                crate::extension::FoldedHook::Proceed(patched) => {
                    let mut c = call.clone();
                    // A `Modify` outcome replaces the hook input `{tool, arguments}`;
                    // carry its `arguments` back onto the call before execution.
                    if let Some(args) = patched.get("arguments") {
                        c.arguments = args.clone();
                    }
                    out.push(c);
                }
            }
        }
        Some(SepToolPlan { calls: out, blocks })
    }

    /// SEP: run the fail-open `tool_result` hook over a completed tool result,
    /// returning the possibly-patched result. Unchanged (and cheap) when no host
    /// is attached. `tool_result` is fail-open, so a block/failure keeps the
    /// original result rather than vetoing (the tool already ran).
    async fn sep_tool_result(&self, call: &crate::tool::ToolCall, result: crate::tool::ToolResult) -> crate::tool::ToolResult {
        let Some(host) = self.extension_host.as_ref() else {
            return result;
        };
        let input = serde_json::json!({
            "tool": call.name,
            "arguments": call.arguments,
            "content": result.content,
            "is_error": result.is_error,
        });
        match host.run_hook(crate::extension::HookType::ToolResult, input).await {
            crate::extension::FoldedHook::Proceed(patch) => {
                let mut r = result;
                if let Some(c) = patch.get("content").and_then(serde_json::Value::as_str) {
                    r.content = c.to_string();
                }
                if let Some(e) = patch.get("is_error").and_then(serde_json::Value::as_bool) {
                    r.is_error = e;
                }
                if let Some(d) = patch.get("details") {
                    r.details = Some(d.clone());
                }
                r
            }
            crate::extension::FoldedHook::Blocked(_) => result,
        }
    }

    /// SEP: fan an observe event out to subscribed extensions. No-op (and cheap)
    /// when no host is attached or nobody subscribed — the `has_subscriber` gate
    /// skips even building the payload's consumers.
    fn sep_dispatch(&self, event: &str, payload: serde_json::Value) {
        if let Some(host) = &self.extension_host {
            if host.has_subscriber(event) {
                host.dispatch_event(event, payload);
            }
        }
    }

    /// SEP: run the fail-open `before_agent_start` hook once at run start, letting
    /// extensions rewrite the system prompt. Applied AFTER `resume_or_new` so it
    /// composes with (never replaces) the resolved persona/system prompt. No-op
    /// when no host is attached. The rewrite is folded across extensions in load
    /// order; a block/failure leaves the prompt unchanged (fail-open).
    async fn sep_before_agent_start(&self, conversation: &mut Conversation) {
        let Some(host) = self.extension_host.as_ref() else {
            return;
        };
        let Some(current) = conversation.messages.iter().find(|m| m.role == Role::System).map(|m| m.content.clone()) else {
            return;
        };
        let rewritten = host.before_agent_start(&current).await;
        if rewritten != current {
            if let Some(msg) = conversation.messages.iter_mut().find(|m| m.role == Role::System) {
                msg.content = rewritten;
            }
        }
    }

    /// SEP: run the fail-open `context` hook, letting extensions replace the
    /// entire message array sent to the LLM this iteration (pi's `context`
    /// middleware analog). Returns `None` — and the caller keeps its borrowed,
    /// zero-copy context — when no host is attached or no extension hooks
    /// `context`; `Some(replacement)` when a hook rewrote it. Fail-open: a
    /// block, failure, or unparseable replacement keeps the original messages.
    ///
    /// Wire shape is the pi-friendly `{role, content}` (the engine's full
    /// `Message` — with its `id`/`timestamp`/`tool_calls` — is deliberately not
    /// exposed so an extension can synthesize messages without forging those
    /// fields). A returned `role` outside system/user/assistant maps to user.
    async fn sep_context_rewrite(&self, context: &[&Message]) -> Option<Vec<Message>> {
        let host = self.extension_host.as_ref()?;
        if !host.any_hook(crate::extension::HookType::Context) {
            return None;
        }
        let wire: Vec<serde_json::Value> = context
            .iter()
            .map(|m| serde_json::json!({ "role": role_wire_name(&m.role), "content": m.content }))
            .collect();
        let input = serde_json::json!({ "messages": wire });
        let crate::extension::FoldedHook::Proceed(patch) = host.run_hook(crate::extension::HookType::Context, input).await else {
            return None;
        };
        let arr = patch.get("messages")?.as_array()?;
        let out: Vec<Message> = arr.iter().filter_map(wire_message_to_message).collect();
        // A non-empty array that parsed to nothing is a malformed rewrite →
        // fail-open (keep the originals). An intentional empty array passes.
        if out.is_empty() && !arr.is_empty() {
            return None;
        }
        Some(out)
    }

    /// Execute a turn's tool calls, honoring the SEP plan, running the
    /// `tool_result` hook on each result, and fanning `tool_execution_*` events
    /// out to subscribed extensions. Shared by the closure-based [`Self::run`]
    /// (`tx = None` → `self.emit`) and the streaming [`Self::run_with_channel`]
    /// (`tx = Some` → channel), so SEP hooks fire identically on both — the
    /// polyglot servers and the TUI all drive the streaming path.
    ///
    /// `blocks` (from [`Self::sep_tool_call_plan`]) forces the sequential path so
    /// each call's veto is honored; without blocks the parallel fast path runs
    /// when configured. `calls` may carry `Modify`-rewritten arguments.
    async fn sep_run_tool_calls(
        &self,
        calls: &[crate::tool::ToolCall],
        blocks: Option<&std::collections::HashMap<String, String>>,
        iteration: u32,
        conversation: &mut Conversation,
        tx: Option<&tokio::sync::mpsc::UnboundedSender<AgentEvent>>,
    ) {
        let send_ev = |ev: AgentEvent| match tx {
            Some(tx) => {
                let _ = tx.send(ev);
            }
            None => self.emit(ev),
        };
        let has_blocks = blocks.is_some_and(|b| !b.is_empty());

        if !has_blocks && self.config.parallel_tools {
            for c in calls {
                send_ev(AgentEvent::ToolCallStart {
                    iteration,
                    tool_name: c.name.clone(),
                    arguments: c.arguments.to_string(),
                });
                self.sep_dispatch(
                    crate::extension::events::TOOL_EXECUTION_START,
                    serde_json::json!({ "iteration": iteration, "tool": c.name }),
                );
            }
            let results = self.tools.execute_parallel(calls).await;
            for (c, result) in calls.iter().zip(results) {
                let result = self.sep_tool_result(c, result).await;
                send_ev(AgentEvent::ToolCallComplete {
                    iteration,
                    tool_name: c.name.clone(),
                    is_error: result.is_error,
                    result: result.content.chars().take(500).collect(),
                    duration_ms: 0,
                });
                self.sep_dispatch(
                    crate::extension::events::TOOL_EXECUTION_END,
                    serde_json::json!({ "iteration": iteration, "tool": c.name, "is_error": result.is_error }),
                );
                conversation.push(Message::tool_result_named(&c.id, &c.name, &result.content));
                self.maybe_checkpoint(conversation, iteration, CheckpointEvent::ToolCallComplete);
            }
        } else {
            for c in calls {
                send_ev(AgentEvent::ToolCallStart {
                    iteration,
                    tool_name: c.name.clone(),
                    arguments: c.arguments.to_string(),
                });
                self.sep_dispatch(
                    crate::extension::events::TOOL_EXECUTION_START,
                    serde_json::json!({ "iteration": iteration, "tool": c.name }),
                );
                let start = std::time::Instant::now();
                let result = if let Some(reason) = blocks.and_then(|b| b.get(&c.id)) {
                    crate::tool::ToolResult {
                        tool_call_id: c.id.clone(),
                        content: format!("blocked by extension: {reason}"),
                        is_error: true,
                        details: None,
                    }
                } else {
                    let r = self.tools.execute(c).await;
                    self.sep_tool_result(c, r).await
                };
                let duration_ms = start.elapsed().as_millis() as u64;
                send_ev(AgentEvent::ToolCallComplete {
                    iteration,
                    tool_name: c.name.clone(),
                    is_error: result.is_error,
                    result: result.content.chars().take(500).collect(),
                    duration_ms,
                });
                self.sep_dispatch(
                    crate::extension::events::TOOL_EXECUTION_END,
                    serde_json::json!({ "iteration": iteration, "tool": c.name, "is_error": result.is_error }),
                );
                conversation.push(Message::tool_result_named(&c.id, &c.name, &result.content));
                self.maybe_checkpoint(conversation, iteration, CheckpointEvent::ToolCallComplete);
            }
        }
    }

    /// Decide whether the latest `LlmResponse` warrants an
    /// `AgentEvent::ModelResolved`. Returns the event when the
    /// gateway's resolved upstream differs from the configured
    /// alias AND hasn't already been reported (or has changed
    /// since the last report); returns `None` otherwise — the
    /// common case where the alias and upstream match (concrete
    /// model selected directly) or nothing has changed.
    ///
    /// Caller is responsible for emitting/sending the event; this
    /// keeps the function callable from both the sync `emit` path
    /// in `run` and the channel-based path in `run_streaming`.
    /// Pearl th-a10c2d.
    /// Spawn the optional fast-model preamble task (see [`AgentConfig::preamble`]).
    /// No-op unless `config.preamble` is set. Best-effort: it runs on its own
    /// task and any failure/slowness is swallowed, so it can never block or
    /// break the real turn. Emits at most one [`AgentEvent::PreambleDelta`].
    ///
    /// Model routing: production (`llm_provider` unset) builds a dedicated fast
    /// client on `preamble.model` (the 20b) sharing the main model's gateway +
    /// key. When a provider is injected (tests), it reuses that provider so
    /// unit tests stay hermetic — the mock's `chat` queue is separate from its
    /// `chat_stream` queue, so it doesn't collide with the main loop. Pearl th-9a5794.
    fn spawn_preamble(&self, user_msg: &str, tx: &tokio::sync::mpsc::UnboundedSender<AgentEvent>) {
        let Some(pre) = &self.config.preamble else { return };
        let provider: Arc<dyn LlmProvider> = match &self.llm_provider {
            Some(p) => Arc::clone(p),
            None => {
                let mut fast_cfg = self.config.llm.clone();
                fast_cfg.model = pre.model.clone();
                fast_cfg.max_tokens = pre.max_tokens;
                Arc::new(LlmClient::new(fast_cfg))
            }
        };
        let sys = Message::system(PREAMBLE_SYSTEM_PROMPT);
        let user = Message::user(user_msg.to_string());
        let tx = tx.clone();
        tokio::spawn(async move {
            match provider.chat(&[&sys, &user], &[]).await {
                Ok(resp) => {
                    let text = resp.content.trim();
                    if !text.is_empty() {
                        let _ = tx.send(AgentEvent::PreambleDelta { content: text.to_string() });
                    }
                }
                // Best-effort: a failed/slow preamble must never surface or block.
                Err(e) => tracing::debug!(error = %e, "preamble generation failed (ignored)"),
            }
        });
    }

    fn model_resolution_event(&self, response: &crate::llm::LlmResponse) -> Option<AgentEvent> {
        let upstream = response.resolved_model.as_deref()?;
        if upstream.is_empty() {
            return None;
        }
        let alias = &self.config.llm.model;
        if alias == upstream {
            // Concrete model selected directly — nothing to surface.
            return None;
        }
        let mut last = self.last_resolved_model.lock().expect("lock last_resolved_model");
        if last.as_deref() == Some(upstream) {
            // Already reported this exact upstream — suppress.
            return None;
        }
        *last = Some(upstream.to_string());
        Some(AgentEvent::ModelResolved {
            alias: alias.clone(),
            upstream: upstream.to_string(),
        })
    }

    /// Drain any pending injected messages from the mailbox channel and push
    /// them onto the conversation as user-turns. Non-blocking: returns
    /// immediately if no channel is wired or nothing is queued. Called at the
    /// top of each iteration of `run` and `run_with_channel`, so messages
    /// arriving mid-LLM-call land before the next request goes out.
    fn drain_injected_messages(&self, conversation: &mut Conversation) {
        let Some(rx) = &self.config.chat_rx else {
            return;
        };
        let Ok(mut guard) = rx.try_lock() else {
            // Another caller is draining (shouldn't happen — single agent loop)
            // — just skip this turn rather than block.
            return;
        };
        loop {
            match guard.try_recv() {
                Ok(InjectedMessage { kind, body }) => {
                    let framed = match kind {
                        InjectedMessageKind::UserChat => body,
                        InjectedMessageKind::LeadGuidance => format!("Lead guidance: {body}"),
                        InjectedMessageKind::AnswerToQuestion => format!("Answer to your question: {body}"),
                    };
                    tracing::info!(kind = ?kind, len = framed.len(), "agent: injected message into conversation");
                    conversation.push(Message::user(framed));
                }
                Err(tokio::sync::mpsc::error::TryRecvError::Empty) => break,
                Err(tokio::sync::mpsc::error::TryRecvError::Disconnected) => {
                    tracing::debug!("agent: chat_rx disconnected");
                    break;
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::checkpoint::MemoryCheckpointStore;
    use std::sync::atomic::{AtomicU32, Ordering};

    fn test_config() -> AgentConfig {
        AgentConfig::new("test-agent", "You are a test agent", LlmConfig::openrouter("fake-key"))
    }

    #[tokio::test]
    async fn run_drives_the_loop_via_injected_llm_provider() {
        use crate::llm_provider::MockLlmClient;

        let mock = MockLlmClient::new();
        mock.push_text("the answer is 42");
        let agent = Agent::new(test_config(), ToolRegistry::new()).with_llm_provider(Arc::new(mock.clone()));

        let convo = agent.run("what is the answer?").await.expect("run completes");

        // A text response with no tool calls ends the loop after one LLM call,
        // and the assistant turn is the mock's scripted content.
        assert_eq!(convo.last_assistant_content(), Some("the answer is 42"));
        assert_eq!(mock.call_count(), 1);
        // The user's message reached the model.
        let calls = mock.calls();
        assert!(calls[0].messages.iter().any(|m| m.content.contains("what is the answer?")));
    }

    #[test]
    fn agent_config_builder() {
        let config = test_config().with_max_iterations(10).with_checkpoint_strategy(CheckpointStrategy::Never);
        assert_eq!(config.max_iterations, 10);
    }

    /// Helper: drain the event channel for the first `PreambleDelta`, waiting up
    /// to 2s for the fire-and-forget preamble task to complete. Returns None if
    /// no preamble arrives before every sender drops.
    async fn recv_preamble(mut rx: tokio::sync::mpsc::UnboundedReceiver<AgentEvent>) -> Option<String> {
        tokio::time::timeout(std::time::Duration::from_secs(2), async {
            loop {
                match rx.recv().await {
                    Some(AgentEvent::PreambleDelta { content }) => return Some(content),
                    Some(_) => {}
                    None => return None,
                }
            }
        })
        .await
        .expect("preamble did not resolve within 2s")
    }

    #[tokio::test]
    async fn preamble_emits_delta_and_does_not_break_the_turn() {
        use crate::llm_provider::MockLlmClient;

        let mock = MockLlmClient::new();
        // Main loop consumes the STREAM queue: one-shot text answer, then Done.
        mock.push_stream(vec![
            StreamEvent::Delta { content: "The answer is 42.".into() },
            StreamEvent::Done { finish_reason: "stop".into() },
        ]);
        // The preamble routes through the SAME mock but its own CHAT queue.
        mock.push_text("Let me look into that.");

        let config = test_config().with_preamble(Some(PreambleConfig::new("groq-gpt-oss-20b")));
        let agent = Agent::new(config, ToolRegistry::new()).with_llm_provider(Arc::new(mock.clone()));

        let (tx, rx) = tokio::sync::mpsc::unbounded_channel::<AgentEvent>();
        let convo = agent.run_with_channel("summarize my week", tx).await.expect("run completes");

        // The real answer still lands — the preamble is additive, not a detour.
        assert_eq!(convo.last_assistant_content(), Some("The answer is 42."));
        // And the ephemeral preamble was emitted from the parallel fast model.
        assert_eq!(recv_preamble(rx).await, Some("Let me look into that.".to_string()));
    }

    #[tokio::test]
    async fn preamble_off_by_default_emits_nothing() {
        use crate::llm_provider::MockLlmClient;

        let mock = MockLlmClient::new();
        mock.push_stream(vec![
            StreamEvent::Delta { content: "hi".into() },
            StreamEvent::Done { finish_reason: "stop".into() },
        ]);
        // No preamble config → spawn_preamble returns early, no chat() call.
        let agent = Agent::new(test_config(), ToolRegistry::new()).with_llm_provider(Arc::new(mock.clone()));

        let (tx, rx) = tokio::sync::mpsc::unbounded_channel::<AgentEvent>();
        agent.run_with_channel("hello", tx).await.expect("run completes");

        assert_eq!(recv_preamble(rx).await, None);
        // Exactly one upstream call (the main chat_stream) — no extra preamble chat.
        assert_eq!(mock.call_count(), 1);
    }

    #[test]
    fn agent_config_model_ceiling_defaults_none_and_sets() {
        assert_eq!(test_config().model_max_output, None);
        assert_eq!(test_config().with_model_ceiling(Some(8_192)).model_max_output, Some(8_192));
        assert_eq!(test_config().with_model_ceiling(None).model_max_output, None);
    }

    // ----- pearl th-operator-verify-rule: with_verify_tests_before_done -----

    #[test]
    fn verify_rule_off_by_default_in_new_config() {
        let cfg = test_config();
        assert!(!cfg.system_prompt.contains(VERIFY_TESTS_RULE_ANCHOR));
    }

    #[test]
    fn verify_rule_appended_when_enabled() {
        let cfg = test_config().with_verify_tests_before_done(true);
        assert!(
            cfg.system_prompt.contains(VERIFY_TESTS_RULE_ANCHOR),
            "system_prompt should contain the verify-rule anchor when enabled: {}",
            cfg.system_prompt
        );
        // Body keywords — bench-runner names must be present so the
        // model can pick the right test command from the workspace
        // shape without us having to wire a workspace-shape detector
        // in Rust.
        assert!(cfg.system_prompt.contains("pytest -q"));
        assert!(cfg.system_prompt.contains("cargo test"));
        assert!(cfg.system_prompt.contains("npm test"));
        assert!(cfg.system_prompt.contains("go test"));
        // Headline rule
        assert!(cfg.system_prompt.to_lowercase().contains("must not produce a final response"));
    }

    #[test]
    fn verify_rule_skipped_when_disabled() {
        let cfg = test_config().with_verify_tests_before_done(false);
        assert!(!cfg.system_prompt.contains(VERIFY_TESTS_RULE_ANCHOR));
    }

    #[test]
    fn verify_rule_preserves_original_system_prompt() {
        let original = "You are a test agent";
        let cfg = test_config().with_verify_tests_before_done(true);
        assert!(
            cfg.system_prompt.starts_with(original),
            "original system_prompt must lead, rule appended after: {}",
            cfg.system_prompt
        );
    }

    #[test]
    fn verify_rule_is_idempotent_on_double_apply() {
        // Calling twice must not stack — otherwise repeated dispatcher
        // configurations could double-tax the context with the rule.
        let cfg = test_config().with_verify_tests_before_done(true).with_verify_tests_before_done(true);
        let anchor_count = cfg.system_prompt.matches(VERIFY_TESTS_RULE_ANCHOR).count();
        assert_eq!(anchor_count, 1, "anchor must appear exactly once after double-apply: {}", cfg.system_prompt);
    }

    #[test]
    fn verify_rule_handles_trailing_newline_in_original_prompt() {
        let mut cfg = AgentConfig::new("test-agent", "You are a test agent\n", LlmConfig::openrouter("fake-key")).with_verify_tests_before_done(true);
        // Should NOT double up the newline.
        assert!(!cfg.system_prompt.contains("\n\n\n"));
        assert!(cfg.system_prompt.contains(VERIFY_TESTS_RULE_ANCHOR));
        // sanity: drain the value to confirm we didn't move ownership
        cfg.system_prompt.clear();
    }

    #[test]
    fn verify_rule_handles_empty_system_prompt() {
        // Defensive: if someone constructs AgentConfig with an empty
        // system prompt, the rule should still apply cleanly.
        let cfg = AgentConfig::new("test-agent", "", LlmConfig::openrouter("fake-key")).with_verify_tests_before_done(true);
        assert!(cfg.system_prompt.contains(VERIFY_TESTS_RULE_ANCHOR));
        assert!(cfg.system_prompt.starts_with('\n') || cfg.system_prompt.starts_with("[verify-tests"));
    }

    #[test]
    fn max_steps_reminder_includes_recovery_scaffold() {
        // The reminder is the model's last guidance before the loop exits.
        // It must instruct a clean wrap-up rather than read like an error
        // — accomplished/remaining/next-steps. Guards against truncating
        // the reminder to "stop, you ran out of steps" which leaves the
        // user with no actionable summary.
        assert!(MAX_STEPS_REMINDER.contains("MAXIMUM"));
        assert!(MAX_STEPS_REMINDER.contains("accomplished"));
        assert!(MAX_STEPS_REMINDER.contains("remaining"));
        assert!(MAX_STEPS_REMINDER.contains("next") || MAX_STEPS_REMINDER.contains("Recommend"));
        // Tools-disabled framing keeps the model from starting a fresh
        // chain that will be cut off mid-flight.
        assert!(MAX_STEPS_REMINDER.contains("text only") || MAX_STEPS_REMINDER.contains("Do not start"));
    }

    #[test]
    fn agent_config_parallel_tools() {
        let config = test_config().with_parallel_tools(true);
        assert!(config.parallel_tools);

        let config = test_config();
        assert!(!config.parallel_tools);
    }

    #[test]
    fn agent_config_with_user_images_sets_pending_images() {
        // Pearl th-25ce5c: a host with a multimodal chat turn stages the
        // images on the config; the agent attaches them to the current
        // user message. Default is empty (text-only turns unchanged).
        use crate::conversation::ImageContent;
        let bare = test_config();
        assert!(bare.next_user_images.is_empty(), "default must be text-only");

        let config = test_config().with_user_images(vec![ImageContent::new("data:image/png;base64,AAAA")]);
        assert_eq!(config.next_user_images.len(), 1);
        assert_eq!(config.next_user_images[0].url, "data:image/png;base64,AAAA");
    }

    #[test]
    fn agent_config_with_prior_messages_seeds_conversation() {
        // Pearl th-422b93: AgentConfig.prior_messages should be
        // pushed into the Conversation after resume_or_new (which
        // installs the system prompt) and before the new user
        // message. Verify the field is wired and order is preserved.
        let prior = vec![Message::user("what repo is this?"), Message::assistant("It's a budgeting app.")];
        let config = test_config().with_prior_messages(prior.clone());
        assert_eq!(config.prior_messages.len(), 2);
        assert!(matches!(config.prior_messages[0].role, Role::User));
        assert_eq!(config.prior_messages[0].content, "what repo is this?");
        assert!(matches!(config.prior_messages[1].role, Role::Assistant));
        assert_eq!(config.prior_messages[1].content, "It's a budgeting app.");

        // Default config has no prior history.
        let bare = test_config();
        assert!(bare.prior_messages.is_empty());
    }

    #[test]
    fn agent_creation() {
        let agent = Agent::new(test_config(), ToolRegistry::new());
        assert!(!agent.id.is_empty());
    }

    #[test]
    fn agent_resume_no_checkpoint() {
        let agent = Agent::new(test_config(), ToolRegistry::new());
        let conv = agent.resume_or_new().expect("resume");
        assert_eq!(conv.len(), 1); // system prompt only
    }

    #[test]
    fn agent_resume_with_checkpoint() {
        let store = Arc::new(MemoryCheckpointStore::new());
        let store_dyn: Arc<dyn CheckpointStore> = Arc::clone(&store) as Arc<dyn CheckpointStore>;
        let agent = Agent::new(test_config(), ToolRegistry::new()).with_checkpoint_store(store_dyn);

        // Save a checkpoint with some messages
        let mut conv = Conversation::new(100_000).with_system_prompt("test");
        conv.push(Message::user("previous message"));
        conv.push(Message::assistant("previous response"));
        let cp = Checkpoint::new(&agent.id, &conv, 5);
        store.save(&cp).expect("save");

        // Resume should restore the conversation
        let restored = agent.resume_or_new().expect("resume");
        assert_eq!(restored.len(), 3); // system + user + assistant
    }

    #[test]
    fn event_handler_receives_events() {
        let count = Arc::new(AtomicU32::new(0));
        let count_clone = Arc::clone(&count);

        let _agent = Agent::new(test_config(), ToolRegistry::new()).with_event_handler(move |_event| {
            count_clone.fetch_add(1, Ordering::Relaxed);
        });

        // Events are emitted during run(), which requires async + real LLM
        // Just verify the handler is set up correctly
        assert_eq!(count.load(Ordering::Relaxed), 0);
    }

    #[test]
    fn agent_event_serialization() {
        let event = AgentEvent::LlmResponse {
            iteration: 3,
            content_preview: "Hello".into(),
            tool_call_count: 2,
        };
        let json = serde_json::to_string(&event).expect("serialize");
        assert!(json.contains("LlmResponse"));
        assert!(json.contains("\"iteration\":3"));
    }

    #[test]
    fn model_resolution_event_emits_when_alias_differs() {
        // Pearl th-a10c2d: when the configured model is a smooth-*
        // alias and the gateway resolves it to a different upstream,
        // the agent should emit one ModelResolved event so the TUI
        // can show `smooth-coding → qwen3-coder-flash`.
        let config = AgentConfig::new("t", "sys", LlmConfig::openrouter("k").with_model("smooth-coding"));
        let agent = Agent::new(config, ToolRegistry::new());

        let resp = crate::llm::LlmResponse {
            content: "ok".into(),
            tool_calls: vec![],
            finish_reason: "stop".into(),
            usage: crate::llm::Usage::default(),
            rate_limit: None,
            gateway_cost_usd: None,
            resolved_model: Some("qwen3-coder-flash".into()),
            reasoning_content: None,
        };

        let event = agent.model_resolution_event(&resp).expect("event on first resolution");
        match event {
            AgentEvent::ModelResolved { alias, upstream } => {
                assert_eq!(alias, "smooth-coding");
                assert_eq!(upstream, "qwen3-coder-flash");
            }
            other => panic!("expected ModelResolved, got {other:?}"),
        }
    }

    #[test]
    fn model_resolution_event_idempotent_on_repeat_resolution() {
        // Same upstream as last time = no event. Pearl th-a10c2d.
        let config = AgentConfig::new("t", "sys", LlmConfig::openrouter("k").with_model("smooth-coding"));
        let agent = Agent::new(config, ToolRegistry::new());

        let resp = crate::llm::LlmResponse {
            content: "ok".into(),
            tool_calls: vec![],
            finish_reason: "stop".into(),
            usage: crate::llm::Usage::default(),
            rate_limit: None,
            gateway_cost_usd: None,
            resolved_model: Some("qwen3-coder-flash".into()),
            reasoning_content: None,
        };

        assert!(agent.model_resolution_event(&resp).is_some(), "first time emits");
        assert!(agent.model_resolution_event(&resp).is_none(), "second turn with same upstream is suppressed");
        assert!(agent.model_resolution_event(&resp).is_none(), "and a third");

        // But if the upstream changes mid-run, we emit again so the
        // status bar updates.
        let resp2 = crate::llm::LlmResponse {
            resolved_model: Some("qwen3-coder-plus".into()),
            ..resp
        };
        let event = agent.model_resolution_event(&resp2).expect("emit on change");
        match event {
            AgentEvent::ModelResolved { upstream, .. } => assert_eq!(upstream, "qwen3-coder-plus"),
            other => panic!("expected ModelResolved, got {other:?}"),
        }
    }

    #[test]
    fn model_resolution_event_suppressed_when_alias_equals_upstream() {
        // Concrete model selected directly — no rewrite happened, no
        // event needed. Pearl th-a10c2d.
        let config = AgentConfig::new("t", "sys", LlmConfig::openrouter("k").with_model("qwen3-coder-flash"));
        let agent = Agent::new(config, ToolRegistry::new());

        let resp = crate::llm::LlmResponse {
            content: "ok".into(),
            tool_calls: vec![],
            finish_reason: "stop".into(),
            usage: crate::llm::Usage::default(),
            rate_limit: None,
            gateway_cost_usd: None,
            resolved_model: Some("qwen3-coder-flash".into()),
            reasoning_content: None,
        };

        assert!(agent.model_resolution_event(&resp).is_none());
    }

    #[test]
    fn model_resolution_event_suppressed_when_response_omits_model() {
        // Older providers may not populate `model` on the response —
        // we must not emit a bogus event in that case. Pearl
        // th-a10c2d.
        let config = AgentConfig::new("t", "sys", LlmConfig::openrouter("k").with_model("smooth-coding"));
        let agent = Agent::new(config, ToolRegistry::new());

        let resp = crate::llm::LlmResponse {
            content: "ok".into(),
            tool_calls: vec![],
            finish_reason: "stop".into(),
            usage: crate::llm::Usage::default(),
            rate_limit: None,
            gateway_cost_usd: None,
            resolved_model: None,
            reasoning_content: None,
        };

        assert!(agent.model_resolution_event(&resp).is_none());
    }

    #[test]
    fn agent_event_variants() {
        let events = vec![
            AgentEvent::Started { agent_id: "a".into() },
            AgentEvent::LlmRequest {
                iteration: 1,
                message_count: 5,
            },
            AgentEvent::ToolCallStart {
                iteration: 1,
                tool_name: "echo".into(),
                arguments: String::new(),
            },
            AgentEvent::ToolCallComplete {
                iteration: 1,
                tool_name: "echo".into(),
                is_error: false,
                result: String::new(),
                duration_ms: 0,
            },
            AgentEvent::CheckpointSaved {
                checkpoint_id: "cp".into(),
                iteration: 1,
            },
            AgentEvent::Completed {
                agent_id: "a".into(),
                iterations: 5,
                cost_usd: 0.042,
                prompt_tokens: 0,
                completion_tokens: 0,
                cached_tokens: 0,
            },
            AgentEvent::MaxIterationsReached { agent_id: "a".into(), max: 50 },
            AgentEvent::BudgetExceeded {
                spent_usd: 5.0,
                limit_usd: 3.0,
            },
            AgentEvent::Error { message: "oops".into() },
            AgentEvent::TokenDelta { content: "hello".into() },
            AgentEvent::StreamingComplete,
            AgentEvent::DelegationStarted {
                parent_id: "p".into(),
                child_id: "c".into(),
                task: "do something".into(),
            },
            AgentEvent::DelegationCompleted {
                parent_id: "p".into(),
                child_id: "c".into(),
                success: true,
            },
        ];
        for event in events {
            let json = serde_json::to_string(&event).expect("serialize");
            assert!(!json.is_empty());
        }
    }

    #[test]
    fn token_delta_event_serialization() {
        let event = AgentEvent::TokenDelta {
            content: "streaming text".into(),
        };
        let json = serde_json::to_string(&event).expect("serialize");
        assert!(json.contains("TokenDelta"));
        assert!(json.contains("streaming text"));
    }

    #[test]
    fn streaming_complete_event_serialization() {
        let event = AgentEvent::StreamingComplete;
        let json = serde_json::to_string(&event).expect("serialize");
        assert!(json.contains("StreamingComplete"));
    }

    // --- Delegation tests ---

    #[tokio::test]
    async fn delegation_handle_is_finished_lifecycle() {
        // Spawn a trivial background task that completes immediately
        let handle = DelegationHandle {
            agent_id: "child-1".into(),
            task: "say hello".into(),
            join_handle: tokio::spawn(async {
                let conv = Conversation::new(100_000).with_system_prompt("test");
                Ok(conv)
            }),
        };

        // Wait for it to finish
        let conv = handle.wait().await.expect("should complete");
        assert_eq!(conv.len(), 1); // system prompt only
    }

    #[test]
    fn sub_agent_config_defaults() {
        let config = SubAgentConfig::default();
        assert_eq!(config.system_prompt, "You are a sub-agent.");
        assert_eq!(config.max_iterations, 10);
        assert!(config.inherit_tools);
    }

    #[tokio::test]
    async fn spawn_sub_agent_creates_unique_id() {
        let parent = Arc::new(Agent::new(test_config(), ToolRegistry::new()));
        let handle1 = parent.spawn_sub_agent("task 1".into(), &SubAgentConfig::default());
        let handle2 = parent.spawn_sub_agent("task 2".into(), &SubAgentConfig::default());

        assert_ne!(handle1.agent_id, handle2.agent_id);
        assert_ne!(handle1.agent_id, parent.id);

        // Clean up
        handle1.cancel();
        handle2.cancel();
    }

    #[tokio::test]
    async fn delegation_started_event_has_correct_ids() {
        let events = Arc::new(Mutex::new(Vec::new()));
        let events_clone = Arc::clone(&events);

        let parent = Arc::new(Agent::new(test_config(), ToolRegistry::new()).with_event_handler(move |event| {
            events_clone.lock().expect("lock").push(event);
        }));

        let parent_id = parent.id.clone();
        let handle = parent.spawn_sub_agent("test task".into(), &SubAgentConfig::default());
        let child_id = handle.agent_id.clone();
        handle.cancel();

        let events = events.lock().expect("lock");
        let started = events.iter().find(|e| matches!(e, AgentEvent::DelegationStarted { .. }));
        assert!(started.is_some(), "DelegationStarted event should be emitted");

        if let Some(AgentEvent::DelegationStarted {
            parent_id: pid,
            child_id: cid,
            task,
        }) = started
        {
            assert_eq!(pid, &parent_id);
            assert_eq!(cid, &child_id);
            assert_eq!(task, "test task");
        }
    }

    #[test]
    fn delegation_completed_event_serialization() {
        let event = AgentEvent::DelegationCompleted {
            parent_id: "parent-123".into(),
            child_id: "child-456".into(),
            success: true,
        };
        let json = serde_json::to_string(&event).expect("serialize");
        assert!(json.contains("DelegationCompleted"));
        assert!(json.contains("parent-123"));
        assert!(json.contains("child-456"));
        assert!(json.contains("true"));
    }

    #[tokio::test]
    async fn cancel_aborts_the_task() {
        let handle = DelegationHandle {
            agent_id: "child-abort".into(),
            task: "long task".into(),
            join_handle: tokio::spawn(async {
                // Simulate a long-running task
                tokio::time::sleep(std::time::Duration::from_secs(60)).await;
                Ok(Conversation::new(100_000))
            }),
        };

        assert!(!handle.is_finished());
        handle.cancel();

        // Give the runtime a moment to propagate the abort
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }

    #[test]
    fn delegation_tool_schema_has_task_parameter() {
        use crate::tool::Tool;

        let parent = Arc::new(Agent::new(test_config(), ToolRegistry::new()));
        let tool = DelegationTool::new(parent);
        let schema = tool.schema();

        assert_eq!(schema.name, "delegate");
        let params = &schema.parameters;
        assert!(params["properties"]["task"].is_object());
        assert_eq!(params["properties"]["task"]["type"], "string");
        let required = params["required"].as_array().expect("required array");
        assert!(required.iter().any(|v| v.as_str() == Some("task")));
    }

    #[test]
    fn drain_pushes_user_chat_verbatim() {
        let (tx, rx) = tokio::sync::mpsc::unbounded_channel::<InjectedMessage>();
        let rx = Arc::new(tokio::sync::Mutex::new(rx));
        let cfg = test_config().with_chat_rx(rx);
        let agent = Agent::new(cfg, ToolRegistry::new());
        let mut conv = Conversation::new(8192).with_system_prompt("sys");

        tx.send(InjectedMessage {
            kind: InjectedMessageKind::UserChat,
            body: "hi from the user".into(),
        })
        .expect("send");

        agent.drain_injected_messages(&mut conv);
        // system + new user-turn
        assert_eq!(conv.len(), 2);
    }

    #[test]
    fn drain_frames_lead_guidance_and_answer() {
        let (tx, rx) = tokio::sync::mpsc::unbounded_channel::<InjectedMessage>();
        let rx = Arc::new(tokio::sync::Mutex::new(rx));
        let cfg = test_config().with_chat_rx(rx);
        let agent = Agent::new(cfg, ToolRegistry::new());
        let mut conv = Conversation::new(8192).with_system_prompt("sys");

        tx.send(InjectedMessage {
            kind: InjectedMessageKind::LeadGuidance,
            body: "focus on auth".into(),
        })
        .expect("send guidance");
        tx.send(InjectedMessage {
            kind: InjectedMessageKind::AnswerToQuestion,
            body: "use port 4400".into(),
        })
        .expect("send answer");

        agent.drain_injected_messages(&mut conv);
        // system + 2 injected
        assert_eq!(conv.len(), 3);
    }

    #[test]
    fn drain_is_noop_when_channel_unset() {
        let agent = Agent::new(test_config(), ToolRegistry::new());
        let mut conv = Conversation::new(8192).with_system_prompt("sys");
        agent.drain_injected_messages(&mut conv);
        assert_eq!(conv.len(), 1); // just system prompt
    }
}
