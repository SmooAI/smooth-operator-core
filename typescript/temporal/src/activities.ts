/**
 * The Temporal **activities** for the agent-turn workflow: the side-effecting
 * half of a turn (the model call, each tool invocation) plus a liveness probe.
 *
 * The TS sibling of the Rust crate's `AgentTurnActivities`. Where the Rust
 * activities read their engine handles from a process-global installed by
 * `init_engine`, the TypeScript Temporal SDK registers activities as an object
 * passed to `Worker.create({ activities })` — so the handles are simply closed
 * over here via {@link createActivities}, no global needed.
 *
 * Each activity runs in the **normal Node context** (not the deterministic
 * workflow sandbox), so it may import the full engine freely: the model call and
 * tool dispatch are a verbatim reuse of the engine's own
 * {@link InProcessActivities} — the same code the in-process executor runs — with
 * the DTO projection applied on the way out. One implementation, two backends.
 */

import { InProcessActivities } from '@smooai/smooth-operator-core/executor';
import type { LlmProvider, Tool, ToolResult } from '@smooai/smooth-operator-core';

import { modelResponseToOutput, type ModelCallInput, type ModelCallOutput, type ToolInvokeInput } from './dto.js';

/**
 * The engine handles the activities run against — the durable-path analog of the
 * arguments the in-process executor is constructed with.
 */
export interface EngineHandles {
    /** The OpenAI-compatible chat client backing the `modelCall` activity. */
    llm: LlmProvider;
    /** The tools dispatchable by the `toolInvoke` activity (default none). */
    tools?: Tool[];
    /** Model id for the request body; defaults to the engine's own default. */
    model?: string;
}

/** The activity surface registered with a Temporal worker. */
export interface AgentTurnActivities {
    /** Liveness probe backing the scaffold health workflow. */
    healthEcho(message: string): Promise<string>;
    /** The model call (`Think`), run as a durable, retried activity. */
    modelCall(input: ModelCallInput): Promise<ModelCallOutput>;
    /** A single tool invocation (`Act`), run as a durable activity. */
    toolInvoke(input: ToolInvokeInput): Promise<ToolResult>;
}

/**
 * Build the activity object a Temporal worker registers, bound to `handles`.
 *
 * Tool *business* failures come back inside {@link ToolResult.isError} (not as a
 * thrown activity error), exactly as {@link InProcessActivities.toolInvoke}
 * reports them — matching the engine's own dispatch so the model reacts to a
 * failed tool instead of the turn failing.
 */
export function createActivities(handles: EngineHandles): AgentTurnActivities {
    const inproc = new InProcessActivities(handles.llm, handles.tools ?? [], handles.model);
    return {
        async healthEcho(message: string): Promise<string> {
            return `smooth-operator-temporal ok: ${message}`;
        },
        async modelCall(input: ModelCallInput): Promise<ModelCallOutput> {
            const response = await inproc.modelCall(input.messages, input.tools);
            return modelResponseToOutput(response);
        },
        async toolInvoke(input: ToolInvokeInput): Promise<ToolResult> {
            return inproc.toolInvoke(input.call);
        },
    };
}
