/**
 * The durable-execution seam (ADR-030, `SMOODEV-1972`/`SMOODEV-1974`) — the
 * TypeScript port of the Rust reference's `executor.rs` + `activities.rs`.
 *
 * It is two seams at two altitudes:
 *
 * - {@link AgentExecutor} decides *where and how* a turn runs, while
 *   {@link SmoothAgent} stays the unit of orchestration (instructions, tools,
 *   loop policy, compaction, checkpointing). The only implementation here is
 *   {@link InProcessExecutor}, which drives the turn in the calling task — a
 *   verbatim delegation to {@link SmoothAgent.run} / {@link SmoothAgent.runStream},
 *   so introducing the abstraction changes no behavior. It needs no
 *   infrastructure and pulls no dependencies: the zero-infra, OSS default.
 * - {@link AgentActivities} + {@link driveTurn} split a turn into its
 *   *side-effecting* half (the model call, each tool invocation) and its
 *   *deterministic* half (the loop that decides "call the model → if it asked
 *   for tools, run them and loop → else stop"). A durable backend runs the
 *   former as retried, memoized workflow activities and replays the latter;
 *   the in-process backend runs both inline. **One loop, two backends** — which
 *   is what keeps the durable path from drifting away from the inline path.
 *
 * The point of the boundary is to let an *optional* durable backend plug in
 * without the engine or its consumers knowing: a turn modeled as a workflow
 * gets crash-safe resume, durable human-in-the-loop via signals, and durable
 * timers. Keeping the executor a thin dispatch seam (rather than reaching into
 * the loop) is deliberate — the battle-tested inline path is untouched, and the
 * finer-grained orchestration/activity split a durable backend needs lives
 * behind {@link driveTurn}, not forced onto the default path.
 *
 * TODO(ADR-030): a Temporal-backed `AgentExecutor` + `AgentActivities` belongs in
 * a SEPARATE package (`@smooai/smooth-operator-temporal`) behind an opt-in entry
 * point — mirroring the Rust reference's separate `smooth-operator-temporal`
 * crate and its off-by-default `temporal` cargo feature — so this published
 * package stays zero-infra and never pulls a Temporal SDK into a consumer's
 * dependency tree.
 *
 * ## Scope of `driveTurn` vs. `SmoothAgent.run`
 *
 * {@link driveTurn} reproduces the *core decision flow* of {@link SmoothAgent.run}:
 * context snapshot → model call → append assistant message → stop-or-run-tools →
 * append tool results → loop to `maxIterations`. It deliberately omits the
 * inline-runtime concerns a durable backend models differently or that are
 * follow-ups in the epic: event emission (workflow history/queries replace it),
 * checkpointing (the event history *is* the checkpoint), compaction, budget
 * enforcement, parallel tools, permission/human gates, knowledge and memory
 * injection. Those layer onto this loop without changing its shape; the
 * convergence goal is for `SmoothAgent.run` itself to delegate here.
 */

import type { AgentRunResponse, ChatClientLike, SmoothAgent, StreamEvent, Tool, ToolCall, ToolResult } from './agent.js';
import type { SmoothAgentThread } from './thread.js';
import type { LlmProvider } from './llmProvider.js';

/**
 * The model's reply, exactly as the OpenAI-compatible client returns it. Named
 * here because {@link AgentActivities.modelCall} both produces and (for a durable
 * backend) serializes it across an activity boundary — it is plain JSON.
 */
export type ModelResponse = Awaited<ReturnType<ChatClientLike['chat']['completions']['create']>>;

/**
 * A backend that executes a {@link SmoothAgent} turn.
 *
 * Implementations decide *how* the turn is driven (inline, or on a durable
 * workflow engine); the agent passed in owns *what* the turn does. Both methods
 * take the agent as a parameter so an executor never owns it — it borrows it for
 * the duration of the turn, exactly like calling {@link SmoothAgent.run} yourself.
 *
 * The parameters mirror the agent's own run entry points, so a consumer can swap
 * a direct `agent.run(...)` for `executor.execute(agent, ...)` with no other
 * change.
 */
export interface AgentExecutor {
    /**
     * Run a single turn for `message`, returning the same {@link AgentRunResponse}
     * {@link SmoothAgent.run} would.
     *
     * Rejects with any fatal error from the underlying turn (model call, tool
     * dispatch, or — for durable backends — the workflow engine).
     */
    execute(agent: SmoothAgent, message: string, history?: Array<Record<string, unknown>>, thread?: SmoothAgentThread): Promise<AgentRunResponse>;

    /**
     * Run a single turn, yielding {@link StreamEvent}s (text deltas, tool calls,
     * tool results) as they occur and ending with the terminal `done` event that
     * carries the final {@link AgentRunResponse}.
     *
     * Behaviorally equivalent to {@link SmoothAgent.runStream} for the in-process
     * backend.
     */
    executeStreaming(agent: SmoothAgent, message: string, history?: Array<Record<string, unknown>>, thread?: SmoothAgentThread): AsyncIterable<StreamEvent>;
}

/**
 * The default, zero-infra executor: it drives the turn inline in the calling
 * task by delegating straight to {@link SmoothAgent.run} / {@link SmoothAgent.runStream}.
 *
 * This is a verbatim pass-through — introducing it changes no behavior. It is
 * the executor used unless a consumer explicitly opts into a durable backend.
 */
export class InProcessExecutor implements AgentExecutor {
    execute(agent: SmoothAgent, message: string, history?: Array<Record<string, unknown>>, thread?: SmoothAgentThread): Promise<AgentRunResponse> {
        return agent.run(message, history, thread);
    }

    async *executeStreaming(
        agent: SmoothAgent,
        message: string,
        history?: Array<Record<string, unknown>>,
        thread?: SmoothAgentThread,
    ): AsyncGenerator<StreamEvent> {
        yield* agent.runStream(message, history, thread);
    }
}

/**
 * The side-effecting boundary of an agent turn: the model call and each tool
 * invocation. A durable backend runs these as workflow activities (retried,
 * memoized in history); the in-process backend runs them inline.
 *
 * Every input and output is a plain, JSON-serializable value — OpenAI-shaped
 * messages, OpenAI-shaped tool specs, the raw model response, a
 * {@link ToolCall} / {@link ToolResult} — so an implementation may marshal them
 * across an activity boundary unchanged.
 */
export interface AgentActivities {
    /**
     * Invoke the model with the given context window and the tool specs to offer
     * it (the OpenAI `tools` array; empty means "no tools").
     *
     * Rejects on a fatal model-call error (network, upstream rejection). A durable
     * backend turns transient failures into activity retries before they surface
     * here.
     */
    modelCall(messages: Array<Record<string, unknown>>, tools: Array<Record<string, unknown>>): Promise<ModelResponse>;

    /**
     * Execute a single tool call, returning its result.
     *
     * Rejects only on a fatal *dispatch* error. A tool's **business** failure is
     * reported via {@link ToolResult.isError} with the message in `content`, not
     * as a rejection — mirroring the Rust engine's `ToolRegistry::execute` and
     * `SmoothAgent`'s own dispatch, both of which feed tool errors back to the
     * model instead of failing the turn.
     */
    toolInvoke(call: ToolCall): Promise<ToolResult>;
}

/** Loop policy for {@link driveTurn}: the bound that keeps the orchestration terminating. */
export interface TurnPolicy {
    /** Maximum model-call iterations before the turn stops unconditionally. */
    maxIterations: number;
}

/**
 * Default iteration bound: the same 8 `SmoothAgent`'s own loop defaults to
 * (`DEFAULTS.maxIterations` in `agent.ts`, which is not exported). The Rust
 * reference defaults to 50 — each language core keeps its own engine's default
 * so swapping in an executor never changes how long a turn may run.
 */
const DEFAULT_MAX_ITERATIONS = 8;

/**
 * Drive one agent turn deterministically over the supplied activity surface,
 * appending to `messages` in place.
 *
 * `messages` must already be seeded with the system prompt, any prior turns, and
 * the current user message — exactly the state {@link SmoothAgent.run} holds when
 * it enters its loop. On return it carries the appended assistant/tool messages.
 *
 * This function performs **no I/O, wall-clock reads, or RNG** of its own — all of
 * that is delegated to `activities` — so it is replay-safe: durable workflow code
 * can run this very function, and the in-process path runs it too.
 *
 * Rejects with the first fatal error from {@link AgentActivities.modelCall} or
 * {@link AgentActivities.toolInvoke}. Exhausting `maxIterations` is NOT an error:
 * the turn stops and `messages` holds what it accumulated, matching
 * {@link SmoothAgent.run}'s post-loop behavior.
 */
export async function driveTurn(
    activities: AgentActivities,
    messages: Array<Record<string, unknown>>,
    tools: Array<Record<string, unknown>>,
    policy: TurnPolicy = { maxIterations: DEFAULT_MAX_ITERATIONS },
): Promise<void> {
    for (let iteration = 1; iteration <= policy.maxIterations; iteration++) {
        // Observe: snapshot the context window so the call can cross an activity
        // boundary without aliasing the live conversation.
        const context = [...messages];

        // Think.
        const response = await activities.modelCall(context, tools);
        const choice = response.choices[0].message;
        const toolCalls = choice.tool_calls ?? [];
        const content = choice.content ?? '';

        // Append the assistant turn (carrying any tool calls), mirroring the Rust
        // engine's push condition exactly. The TS model response carries no
        // `reasoning_content`, so that third disjunct has no analogue here.
        if (content !== '' || toolCalls.length > 0) {
            const assistantMsg: Record<string, unknown> = { role: 'assistant', content };
            if (toolCalls.length > 0) {
                assistantMsg.tool_calls = toolCalls.map((tc) => ({
                    id: tc.id,
                    type: 'function',
                    function: { name: tc.function.name, arguments: tc.function.arguments },
                }));
            }
            messages.push(assistantMsg);
        }

        // No tool calls ⇒ the agent is done.
        if (toolCalls.length === 0) return;

        // Act: run each tool and append its result, paired by call id + name so the
        // model can match results to calls.
        for (const tc of toolCalls) {
            let args: Record<string, unknown>;
            try {
                args = tc.function.arguments ? (JSON.parse(tc.function.arguments) as Record<string, unknown>) : {};
            } catch {
                // Malformed model output: report it to the model the way a failed
                // tool would be, without spending an invocation on it.
                messages.push({
                    role: 'tool',
                    tool_call_id: tc.id,
                    name: tc.function.name,
                    content: `error: tool '${tc.function.name}' received invalid JSON arguments`,
                });
                continue;
            }
            const result = await activities.toolInvoke({ id: tc.id, name: tc.function.name, arguments: args });
            messages.push({ role: 'tool', tool_call_id: tc.id, name: tc.function.name, content: result.content });
        }
    }
}

/**
 * The default, zero-infra {@link AgentActivities}: the model call goes through an
 * {@link LlmProvider} and each tool call through the supplied tool set, inline in
 * the calling task.
 */
export class InProcessActivities implements AgentActivities {
    private readonly toolsByName: Map<string, Tool>;

    /**
     * @param llm    the OpenAI-compatible chat client to call.
     * @param tools  the tools dispatchable in this turn.
     * @param model  model id for the request body; defaults to the same model
     *               `SmoothAgent` defaults to (`DEFAULTS.model` in `agent.ts`).
     */
    constructor(
        private readonly llm: LlmProvider,
        tools: Tool[] = [],
        private readonly model = 'claude-haiku-4-5',
    ) {
        this.toolsByName = new Map(tools.map((t) => [t.name, t]));
    }

    modelCall(messages: Array<Record<string, unknown>>, tools: Array<Record<string, unknown>>): Promise<ModelResponse> {
        return this.llm.chat.completions.create({
            model: this.model,
            messages,
            ...(tools.length > 0 ? { tools } : {}),
        });
    }

    async toolInvoke(call: ToolCall): Promise<ToolResult> {
        // Tool failures are reported via `isError` rather than a rejection, so
        // dispatch itself is effectively infallible here (Rust parity).
        const tool = this.toolsByName.get(call.name);
        if (!tool) return { toolCallId: call.id, content: `error: unknown tool '${call.name}'`, isError: true };
        try {
            return { toolCallId: call.id, content: await tool.execute(call.arguments), isError: false };
        } catch (err) {
            return {
                toolCallId: call.id,
                content: `error: tool '${call.name}' failed: ${err instanceof Error ? err.message : String(err)}`,
                isError: true,
            };
        }
    }
}
