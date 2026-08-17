/**
 * The Temporal **workflows** (the deterministic half of durable execution).
 *
 * The TS sibling of the Rust crate's `AgentTurnWorkflow` + `HealthWorkflow`. An
 * agent turn runs as {@link agentTurnWorkflow}, which drives the engine's
 * deterministic {@link driveTurn} orchestration **unchanged** over an adapter
 * that schedules the model call and each tool invocation as Temporal activities.
 * The in-process executor runs the same `driveTurn` inline — one loop, two
 * backends.
 *
 * This module runs in Temporal's **deterministic workflow sandbox**, so it may
 * import only replay-safe code: `@temporalio/workflow` primitives, the pure
 * `driveTurn` + its types from `@smooai/smooth-operator-core/executor` (that
 * entry point has zero runtime imports — it is type-only), and the plain DTO
 * helpers. It performs no I/O, clock reads, or RNG of its own — every
 * side-effect is a scheduled activity, every wait a durable `sleep`/`condition`.
 */

import { condition, proxyActivities, setHandler, sleep } from '@temporalio/workflow';

import { driveTurn } from '@smooai/smooth-operator-core/executor';
import type { AgentActivities } from '@smooai/smooth-operator-core/executor';
// Type-only (erased at compile) so the workflow bundle pulls no core runtime code.
import type { ToolCall, ToolResult } from '@smooai/smooth-operator-core';

import type { AgentTurnActivities } from './activities.js';
import { outputToModelResponse, type AgentTurnInput, type AgentTurnResult } from './dto.js';
import { approveToolSignal, denyToolSignal } from './signals.js';

/**
 * Activity stubs. 60s start-to-close mirrors the Rust crate's
 * `agent_activity_opts`; the SDK's default retry policy makes transient model /
 * tool failures durable retries rather than turn failures.
 */
const { modelCall, toolInvoke, healthEcho } = proxyActivities<AgentTurnActivities>({
    startToCloseTimeout: '60s',
});

/**
 * Run one agent turn as a durable workflow. Seeds the conversation (system +
 * history + user), then drives {@link driveTurn} over an activity-scheduling
 * adapter, and returns the full conversation plus turn accounting.
 *
 * Durable human-in-the-loop and durable timers live in the adapter, not in
 * `driveTurn`: an approval-gated tool blocks on {@link condition} until an
 * `approveTool` / `denyTool` signal names its call id, and the configured wait
 * tool sleeps on a durable {@link sleep} timer — both recorded in workflow
 * history, so they survive worker restarts and can resolve arbitrarily later.
 */
export async function agentTurnWorkflow(input: AgentTurnInput): Promise<AgentTurnResult> {
    const approved = new Set<string>();
    const denied = new Set<string>();
    setHandler(approveToolSignal, (callId: string) => {
        approved.add(callId);
    });
    setHandler(denyToolSignal, (callId: string) => {
        denied.add(callId);
    });

    const messages: Array<Record<string, unknown>> = [];
    if (input.systemPrompt) messages.push({ role: 'system', content: input.systemPrompt });
    if (input.history) messages.push(...input.history);
    messages.push({ role: 'user', content: input.userMessage });

    let iterations = 0;
    let toolCalls = 0;
    const usage = { prompt_tokens: 0, completion_tokens: 0 };
    const approvalRequired = input.approvalRequiredTools ?? [];

    const activities: AgentActivities = {
        async modelCall(msgs, tools) {
            iterations += 1;
            const output = await modelCall({ messages: msgs, tools });
            if (output.usage) {
                usage.prompt_tokens += output.usage.prompt_tokens;
                usage.completion_tokens += output.usage.completion_tokens;
            }
            return outputToModelResponse(output);
        },
        async toolInvoke(call: ToolCall): Promise<ToolResult> {
            // Durable wait: the configured wait tool sleeps on a Temporal timer
            // instead of dispatching an activity. Recorded in history, so the
            // pause survives restarts and can span days.
            if (input.waitTool !== undefined && call.name === input.waitTool) {
                const raw = call.arguments.seconds;
                const seconds = typeof raw === 'number' ? raw : Number(raw);
                if (!Number.isInteger(seconds) || seconds < 0) {
                    return { toolCallId: call.id, content: `wait tool '${call.name}' requires an integer 'seconds' argument`, isError: true };
                }
                await sleep(seconds * 1000);
                return { toolCallId: call.id, content: `Waited ${seconds}s (durable timer).`, isError: false };
            }

            // Durable human-in-the-loop gate: block until a signal names this call
            // id. `driveTurn` is unchanged — the gate lives here.
            if (approvalRequired.includes(call.name)) {
                await condition(() => approved.has(call.id) || denied.has(call.id));
                if (denied.has(call.id)) {
                    return { toolCallId: call.id, content: `Tool call '${call.name}' was denied by human approval.`, isError: true };
                }
            }

            toolCalls += 1;
            return toolInvoke({ call });
        },
    };

    if (input.maxIterations !== undefined && input.maxIterations > 0) {
        await driveTurn(activities, messages, input.tools ?? [], { maxIterations: input.maxIterations });
    } else {
        // Omit the policy so the engine's own default iteration bound applies —
        // never a magic number duplicated here.
        await driveTurn(activities, messages, input.tools ?? []);
    }

    return { messages, iterations, toolCalls, usage };
}

/**
 * Scaffold liveness workflow — proves the SDK integrates end to end and backs the
 * health e2e. The TS sibling of the Rust crate's `HealthWorkflow`.
 */
export async function healthWorkflow(message: string): Promise<string> {
    return healthEcho(message);
}
