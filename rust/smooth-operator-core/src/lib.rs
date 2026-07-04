//! # Smooth Operator
//!
//! An embeddable, polyglot AI agent engine with built-in checkpointing, tool
//! system, and LLM client.
//!
//! Inspired by LangGraph, CrewAI, and Agno — purpose-built for orchestrated
//! agent workloads with security-first design.

pub mod activities;
pub mod agent;
pub mod cast;
pub mod checkpoint;
pub mod context;
pub mod conversation;
pub mod cost;
pub mod executor;
pub mod extension;
pub mod human;
pub mod knowledge;
pub mod llm;
pub mod llm_provider;
pub mod memory;
pub mod permission;
pub mod providers;
pub mod quirks;
pub mod resolution;
pub mod tool;
pub mod tool_search;
pub mod workflow;
pub mod ws_resilience;

pub use activities::{drive_turn, AgentActivities, InProcessActivities, TurnPolicy};
pub use agent::{Agent, AgentConfig, AgentEvent, DelegationHandle, DelegationTool, SubAgentConfig};
pub use cast::{Cast, Clearance, DispatchResult, DispatchSubagentTool, LlmConfigFactory, OperatorRole, PermissionHook, RoleKind};
pub use checkpoint::{Checkpoint, CheckpointStore, MemoryCheckpointStore};
pub use conversation::{CompactionResult, CompactionStrategy, Conversation, Message, Role};
pub use cost::{BudgetExceeded, CostBudget, CostEntry, CostTracker, ModelPricing};
pub use executor::{AgentExecutor, InProcessExecutor};
pub use extension::{ExtensionHost, ExtensionLlmProvider, ExtensionManifest, ExtensionTool, FoldedHook, HookType, HostDelegate, ProviderRegistration};
pub use human::{human_channel, ConfirmationHook, HumanChannelPair, HumanRequest, HumanResponse};
pub use knowledge::{Document, DocumentType, InMemoryKnowledge, KnowledgeBase, KnowledgeResult};
pub use llm::{accumulate_stream_events, LlmClient, LlmConfig, LlmResponse, ResponseFormat, StreamEvent};
pub use memory::{InMemoryMemory, Memory, MemoryEntry, MemoryType};
// `permission::PermissionHook` is the dangerous-command classifier gate for
// SEP extension (and native) tool calls; it is intentionally NOT re-exported
// at the crate root because `cast::PermissionHook` (role clearance) already
// owns that name. Reach it via `smooth_operator_core::permission::PermissionHook`.
pub use permission::{AutoMode, Verdict};
pub use providers::{Activity, ModelRouting, ModelSlot, ProviderConfig, ProviderRegistry};
pub use tool::{Tool, ToolCall, ToolRegistry, ToolResult, ToolSchema};
pub use workflow::{EdgeTarget, FnNode, Node, State, Workflow, WorkflowBuilder};
pub use ws_resilience::{ConnectionManager, ConnectionState, MessageBuffer, ResiliencyConfig};
