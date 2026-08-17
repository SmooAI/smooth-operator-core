/**
 * @smooai/smooth-operator-core — a native, in-process agent engine for TypeScript.
 *
 * The Phase-0 TypeScript sibling of the Rust reference engine, the C# core, and
 * the Python core: an agentic tool-calling loop over any OpenAI-compatible chat
 * client, with in-memory knowledge grounding. See `docs/Architecture/TypeScript Core.md`.
 */

export { delegateTool, effectiveMaxTokens, SmoothAgent } from './agent.js';
export type { AgentOptions, AgentRunResponse, ChatChunk, ChatClientLike, ExtensionFold, ExtensionHooks, StreamEvent, Tool, ToolCall, ToolHook, ToolResult } from './agent.js';
export { Cast, Clearance, makeRole, RoleKind } from './cast.js';
export type { OperatorRole } from './cast.js';
export { InMemoryCheckpointStore } from './checkpoint.js';
export type { Checkpoint, CheckpointStore } from './checkpoint.js';
export { extractSection, findProjectContextFile, headingToAnchor, loadProjectContext, parseFileReferences, parseLinkLine } from './context.js';
export type { FileRef } from './context.js';
export { applyCacheControl, supportsAnthropicCacheControl } from './cacheControl.js';
export { CostTracker, DEFAULT_PRICING, GATEWAY_COST_HEADERS, parseGatewayCost } from './cost.js';
export type { CostBudget, HeaderLike, ModelPricing, Usage } from './cost.js';
export { driveTurn, InProcessActivities, InProcessExecutor } from './executor.js';
export type { AgentActivities, AgentExecutor, ModelResponse, TurnPolicy } from './executor.js';
export { createGatewayClient, gatewayClientFrom } from './gatewayClient.js';
export type { GatewayClientOptions } from './gatewayClient.js';
export { approve, approveAlways, deny, HumanDecision, isApproved } from './humanGate.js';
export type { HumanApprovalRequest, HumanApprovalResponse, HumanGate } from './humanGate.js';
export {
    AutoMode,
    autoModeFromEnv,
    autoModeFromValue,
    decide,
    domainMatchesSuffixList,
    extractHosts,
    hostFromToken,
    PermissionHook,
    splitCompound,
    stripWrappersAndSudo,
    toolCategory,
} from './permission.js';
export type { Category, Verdict } from './permission.js';
export { hostMatchesGlob, PermissionGrants } from './permissionGrants.js';
export type { GrantQuery } from './permissionGrants.js';
export { DenyPolicy, DenyRules, globMatch } from './denyPolicy.js';
export type { DenyPredicate } from './denyPolicy.js';
export { InMemoryKnowledge } from './knowledge.js';
export type { Knowledge, KnowledgeHit } from './knowledge.js';
export { MockLlmProvider, SCRIPTED_USAGE, textResponse, toolCallResponse } from './llmProvider.js';
export type { LlmProvider, RecordedCall, ScriptedMessage, ScriptedUsage } from './llmProvider.js';
export { InMemoryMemory } from './memory.js';
export { PROMPT_CACHE_BOUNDARY, PromptCache } from './promptCache.js';
export type { Memory, MemoryEntry } from './memory.js';
export { hasInjection, hasSecrets, NarcHook, redactMatch, scanInjection, scanSecrets, Severity, severityLabel } from './narc.js';
export type { NarcAlert, NarcFinding } from './narc.js';
export { LexicalReranker, NoopReranker } from './rerank.js';
export type { Reranker } from './rerank.js';
export { jsonSchemaFormat, responseFormatField, structuredJson } from './structured.js';
export type { ResponseFormat } from './structured.js';
export { SmoothAgentThread } from './thread.js';
export { MAX_MATCHES, TOOL_SEARCH_NAME, ToolSearch } from './toolSearch.js';
export { HashEmbedder, hashToken, VectorKnowledge } from './vector.js';
export type { Embedder } from './vector.js';
export { END, subWorkflowNode, Workflow, WorkflowError } from './workflow.js';
export type { NodeFn, Router } from './workflow.js';
