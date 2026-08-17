/**
 * Data-transfer objects for the Temporal **activity boundary**.
 *
 * The TypeScript port of the Rust crate's `dto.rs`. A Temporal activity
 * serializes its input and output as JSON, so the workflow-side
 * {@link import('@smooai/smooth-operator-core').AgentActivities} surface cannot
 * traffic the raw {@link ModelResponse} (the full OpenAI object) across the
 * boundary directly — it traffics {@link ModelCallOutput}, a projection of the
 * fields the deterministic `driveTurn` orchestration actually reads (`content`,
 * `tool_calls`) plus the accounting fields (`usage`), and the workflow-side
 * adapter reconstructs a minimal {@link ModelResponse} from it via
 * {@link outputToModelResponse}.
 *
 * Everything here is plain data over `@smooai/smooth-operator-core` types and
 * imports **no** `@temporalio/*` package, so it is always built and unit-tested
 * regardless of whether a Temporal runtime is present — exactly like the Rust
 * `dto.rs` compiling without the `temporal` cargo feature.
 */

import type { ModelResponse } from '@smooai/smooth-operator-core/executor';
import type { ToolCall } from '@smooai/smooth-operator-core';

/** Token usage carried across the activity boundary (OpenAI wire spelling). */
export interface DtoUsage {
    prompt_tokens: number;
    completion_tokens: number;
}

/** Input to the `modelCall` activity: the context window + the tool specs to offer. */
export interface ModelCallInput {
    /** The conversation context window, as OpenAI-shaped messages. */
    messages: Array<Record<string, unknown>>;
    /** Tool schemas (the OpenAI `tools` array) the model may call; empty means none. */
    tools: Array<Record<string, unknown>>;
}

/**
 * Output of the `modelCall` activity: a serde projection of {@link ModelResponse}.
 *
 * Carries the fields the orchestration reads (`content`, `toolCalls`) plus the
 * accounting `usage` so the durable path preserves cost/audit data. Anything the
 * live OpenAI response carries beyond these is transient and dropped — it does
 * not survive (nor need to survive) the activity boundary.
 */
export interface ModelCallOutput {
    /** Assistant text content ('' when the model only asked for tools). */
    content: string;
    /** Tool calls the model requested, in OpenAI wire shape. */
    toolCalls: Array<{ id: string; function: { name: string; arguments: string } }>;
    /** Token usage reported by the gateway, if any. */
    usage?: DtoUsage;
}

/** Input to the `toolInvoke` activity: the single tool call to execute. */
export interface ToolInvokeInput {
    /** The tool call (id + name + arguments) to dispatch. */
    call: ToolCall;
}

/**
 * Input to the agent-turn workflow — everything needed to seed the conversation
 * and bound the loop. Serializable so it crosses the workflow-start boundary. The
 * TS sibling of Rust's `AgentTurnInput`.
 */
export interface AgentTurnInput {
    /** System prompt for the turn. */
    systemPrompt: string;
    /** The user message that opens the turn. */
    userMessage: string;
    /** Prior OpenAI-shaped messages replayed as memory before the user message. */
    history?: Array<Record<string, unknown>>;
    /** Tool schemas (the OpenAI `tools` array) available to the model. */
    tools?: Array<Record<string, unknown>>;
    /** Iteration bound. Omitted / `0` falls back to the loop's default. */
    maxIterations?: number;
    /**
     * Names of tools that require human approval before they run. When the model
     * calls one, the workflow blocks durably until an `approveTool` / `denyTool`
     * signal names that tool call.
     */
    approvalRequiredTools?: string[];
    /**
     * Name of the built-in **durable wait** tool, if any. A model call to this tool
     * with an integer `seconds` argument sleeps the workflow on a Temporal timer
     * (a durable pause that can span days) instead of dispatching an activity.
     */
    waitTool?: string;
}

/**
 * Result of the agent-turn workflow: the full conversation plus the turn
 * accounting the durable path preserved. Serializable (it is the workflow's
 * return value).
 */
export interface AgentTurnResult {
    /** The full conversation (system + user + assistant/tool messages). */
    messages: Array<Record<string, unknown>>;
    /** Number of model-call iterations the loop ran. */
    iterations: number;
    /** Number of tools actually invoked (approval-denied / wait tools excluded). */
    toolCalls: number;
    /** Accumulated token usage across every model call. */
    usage: DtoUsage;
}

/**
 * Project a live {@link ModelResponse} down to the serde {@link ModelCallOutput}
 * the activity returns. The TS sibling of Rust's `impl From<&LlmResponse> for
 * ModelCallOutput`.
 */
export function modelResponseToOutput(response: ModelResponse): ModelCallOutput {
    const message = response.choices[0].message;
    const usage = response.usage ?? undefined;
    const out: ModelCallOutput = {
        content: message.content ?? '',
        toolCalls: (message.tool_calls ?? []).map((tc) => ({
            id: tc.id,
            function: { name: tc.function.name, arguments: tc.function.arguments },
        })),
    };
    if (usage) {
        out.usage = {
            prompt_tokens: usage.prompt_tokens ?? 0,
            completion_tokens: usage.completion_tokens ?? 0,
        };
    }
    return out;
}

/**
 * Reconstruct the minimal {@link ModelResponse} the deterministic `driveTurn`
 * loop consumes (`choices[0].message.{content,tool_calls}` + `usage`) from the
 * activity's {@link ModelCallOutput}. The TS sibling of Rust's
 * `ModelCallOutput::into_llm_response`.
 */
export function outputToModelResponse(output: ModelCallOutput): ModelResponse {
    return {
        choices: [
            {
                message: {
                    content: output.content,
                    tool_calls: output.toolCalls.length > 0 ? output.toolCalls : null,
                },
            },
        ],
        usage: output.usage ?? null,
    } as ModelResponse;
}
