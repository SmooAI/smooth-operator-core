/**
 * The Temporal-backed {@link AgentExecutor} — the durable sibling of the engine's
 * zero-infra `InProcessExecutor`.
 *
 * It runs a turn by starting the {@link agentTurnWorkflow} on a Temporal cluster
 * and awaiting its result, then shaping the returned conversation into the same
 * {@link AgentRunResponse} `SmoothAgent.run` would produce. A consumer swaps a
 * direct `agent.run(...)` for `executor.execute(agent, ...)` with no other change
 * — the turn now survives a crash, gets durable HITL via signals, and can pause
 * on a durable timer.
 *
 * ## The engine-handle split (mirrors Rust's `init_engine`)
 *
 * A durable turn has two halves in two processes: the **worker** holds the model
 * client + tool *implementations* (via {@link createActivities}); this **executor**
 * (client side) holds the turn *configuration* — system prompt, tool *schemas*,
 * approval/wait policy — supplied at construction. That is why {@link execute}
 * reads its prompt/tools from {@link TemporalAgentExecutorOptions} rather than off
 * the passed `agent` (whose config is private and lives in the worker process).
 * `message` and `history` ARE honored per call.
 *
 * The workflow is started by its **type name** (never by importing the workflow
 * module), so this client-side file pulls in no `@temporalio/workflow` sandbox
 * code — importing that module in a normal Node context throws.
 */

import { randomUUID } from 'node:crypto';

import type { Client } from '@temporalio/client';

import type { AgentExecutor, AgentRunResponse, SmoothAgent, StreamEvent } from '@smooai/smooth-operator-core';
import type { SmoothAgentThread } from '@smooai/smooth-operator-core';

import type { AgentTurnInput, AgentTurnResult } from './dto.js';

/** The Temporal workflow type name the worker registers {@link agentTurnWorkflow} under. */
export const AGENT_TURN_WORKFLOW_TYPE = 'agentTurnWorkflow';

/** Configuration for a {@link TemporalAgentExecutor}. */
export interface TemporalAgentExecutorOptions {
    /** A connected `@temporalio/client` {@link Client}. */
    client: Client;
    /** The task queue the worker polls (must match the worker's). */
    taskQueue: string;
    /** System prompt for every turn this executor runs. */
    systemPrompt?: string;
    /** OpenAI tool schemas (the `tools` array) offered to the model. */
    tools?: Array<Record<string, unknown>>;
    /** Iteration bound; omitted / `0` uses the engine default. */
    maxIterations?: number;
    /** Tool names gated behind durable human approval (`approveTool` / `denyTool` signals). */
    approvalRequiredTools?: string[];
    /** Name of the durable wait tool, if any. */
    waitTool?: string;
    /** Prefix for generated workflow ids (default `agent-turn`). */
    workflowIdPrefix?: string;
}

export class TemporalAgentExecutor implements AgentExecutor {
    constructor(private readonly options: TemporalAgentExecutorOptions) {}

    async execute(
        _agent: SmoothAgent,
        message: string,
        history?: Array<Record<string, unknown>>,
        _thread?: SmoothAgentThread,
    ): Promise<AgentRunResponse> {
        const input: AgentTurnInput = {
            systemPrompt: this.options.systemPrompt ?? '',
            userMessage: message,
            history,
            tools: this.options.tools,
            maxIterations: this.options.maxIterations,
            approvalRequiredTools: this.options.approvalRequiredTools,
            waitTool: this.options.waitTool,
        };
        const workflowId = `${this.options.workflowIdPrefix ?? 'agent-turn'}-${randomUUID()}`;
        const result: AgentTurnResult = await this.options.client.workflow.execute(AGENT_TURN_WORKFLOW_TYPE, {
            taskQueue: this.options.taskQueue,
            workflowId,
            args: [input],
        });
        return toAgentRunResponse(result);
    }

    /**
     * A workflow-backed turn has no token-delta stream (its history is the
     * checkpoint, not a live channel), so streaming resolves the durable turn and
     * yields a single terminal `done`.
     *
     * ponytail: no incremental `text` / `tool_call` events on the durable path —
     * a durable turn's progress is observed via workflow history/queries, not a
     * token stream. Upgrade path (per ADR-030's open questions): a workflow-side
     * signal/update channel feeding deltas back to the client.
     */
    async *executeStreaming(
        agent: SmoothAgent,
        message: string,
        history?: Array<Record<string, unknown>>,
        thread?: SmoothAgentThread,
    ): AsyncGenerator<StreamEvent> {
        const response = await this.execute(agent, message, history, thread);
        yield { type: 'done', response };
    }
}

/** Shape a durable-turn {@link AgentTurnResult} into an engine {@link AgentRunResponse}. */
function toAgentRunResponse(result: AgentTurnResult): AgentRunResponse {
    let text = '';
    for (let i = result.messages.length - 1; i >= 0; i--) {
        const msg = result.messages[i];
        if (msg.role === 'assistant' && typeof msg.content === 'string' && msg.content.length > 0) {
            text = msg.content;
            break;
        }
    }
    return {
        text,
        iterations: result.iterations,
        toolCalls: result.toolCalls,
        usage: { promptTokens: result.usage.prompt_tokens, completionTokens: result.usage.completion_tokens },
        // ponytail: authoritative per-request cost lives in the gateway's spend
        // logs, not carried back through the workflow boundary. Wire it via the
        // model_call DTO's gateway cost when durable-turn cost attribution lands.
        costUsd: 0,
        budgetExceeded: false,
    };
}
