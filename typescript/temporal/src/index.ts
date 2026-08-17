/**
 * @smooai/smooth-operator-temporal — optional Temporal-backed durable execution
 * for the TypeScript smooth-operator engine (ADR-030).
 *
 * The TypeScript sibling of the Rust `smooth-operator-temporal` crate. An agent
 * turn runs as a Temporal workflow ({@link agentTurnWorkflow}) whose model call
 * and tool invocations are activities, driving the engine's deterministic
 * `driveTurn` loop unchanged — the same loop the in-process executor runs.
 *
 * - {@link TemporalAgentExecutor} — the durable {@link AgentExecutor}; a
 *   drop-in for the engine's `InProcessExecutor`.
 * - {@link createActivities} — build the worker's activity object from engine
 *   handles (model client + tools).
 * - Workflows are also published at the `./workflows` subpath (the path a
 *   Temporal worker registers) and activities at `./activities`.
 * - The `./dto` boundary types (re-exported here) carry no Temporal dependency.
 */

export type { AgentTurnActivities, EngineHandles } from './activities.js';
export { createActivities } from './activities.js';
export type { AgentTurnInput, AgentTurnResult, DtoUsage, ModelCallInput, ModelCallOutput, ToolInvokeInput } from './dto.js';
export { modelResponseToOutput, outputToModelResponse } from './dto.js';
export { AGENT_TURN_WORKFLOW_TYPE, TemporalAgentExecutor } from './executor.js';
export type { TemporalAgentExecutorOptions } from './executor.js';
// Signals are exported from their own context-free module so a client can import
// them; the workflow functions themselves live at the `./workflows` subpath (the
// path a Temporal worker registers) and are NOT re-exported here — importing that
// module in a normal Node process throws (its top-level `proxyActivities`).
export { approveToolSignal, denyToolSignal } from './signals.js';
