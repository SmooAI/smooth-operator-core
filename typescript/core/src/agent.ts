/**
 * The TypeScript smooth-operator core: a native agentic loop.
 *
 * Phase-0 sibling of the C# `SmoothAgent` (`dotnet/core`), the Python core
 * (`python/core`), and the Rust reference engine. Drives an agentic tool-calling
 * loop over any OpenAI-compatible chat client (the `openai` SDK pointed at a
 * gateway): inject retrieved knowledge, call the model, run any requested tools,
 * feed results back, and loop until the model answers without a tool call or the
 * iteration budget is hit.
 *
 * Deliberately minimal (no compaction / budget / checkpointing yet) — those layer
 * on exactly as they did when the C# core grew past Phase 0.
 */

import type { Clearance } from './cast.js';
import type { CheckpointStore } from './checkpoint.js';
import type { SmoothAgentThread } from './thread.js';
import type { Memory } from './memory.js';
import type { Reranker } from './rerank.js';
import { compact } from './compaction.js';
import { CostTracker } from './cost.js';
import type { CostBudget, ModelPricing, Usage } from './cost.js';
import type { HumanGate } from './humanGate.js';
import { isApproved } from './humanGate.js';
import type { Knowledge } from './knowledge.js';
import { ToolSearch } from './toolSearch.js';

/** A callable tool the agent may invoke. Mirrors the reference engines' tool seam. */
export interface Tool {
    name: string;
    description: string;
    /** JSON Schema for the tool's arguments. */
    parameters: Record<string, unknown>;
    execute(args: Record<string, unknown>): Promise<string>;
}

export interface AgentOptions {
    instructions?: string;
    model?: string;
    maxIterations?: number;
    maxTokens?: number;
    temperature?: number;
    knowledge?: Knowledge;
    knowledgeTopK?: number;
    /** Reranker applied to retrieved hits before injection (default: passthrough). */
    reranker?: Reranker;
    /** Candidate pool size to retrieve before reranking; when > knowledgeTopK, more docs are fetched, reranked, then trimmed. */
    knowledgeCandidateK?: number;
    /** Optional long-term memory; relevant entries are recalled into context each turn. */
    memory?: Memory;
    /** How many memory entries to recall per turn (default 4). */
    memoryTopK?: number;
    tools?: Tool[];
    /**
     * When `true` and an assistant turn returns ≥2 tool calls, dispatch them
     * concurrently (`Promise.all`) instead of sequentially. The tool-result
     * messages are still appended in the original `tool_calls` order, so the
     * transcript stays deterministic regardless of completion order. Default
     * `false` preserves the sequential behaviour. Per-tool semantics (clearance,
     * human-gate approval, tool_search promotion, JSON parsing, error handling)
     * are unchanged — only the dispatch loop runs in parallel.
     */
    parallelToolCalls?: boolean;
    /**
     * Deferred tools — registered but with their schemas HIDDEN from the model.
     * When any are present, a built-in `tool_search` meta-tool is advertised in
     * their place; the model calls it to fuzzy-match and promote the ones it needs,
     * which then become visible + dispatchable on subsequent turns. Keeps the tool
     * schema payload small when there are many rarely-used tools. An unpromoted
     * deferred tool is NOT dispatchable.
     */
    deferredTools?: Tool[];
    /**
     * Approximate token budget for the context window. Before each model call,
     * older non-system messages are dropped (sliding window) to stay under it.
     * `0` disables compaction. Defaults to 8000.
     */
    maxContextTokens?: number;
    /** Optional ceiling for the turn (token and/or USD). The turn stops early once a model call pushes usage/cost over the budget. */
    budget?: CostBudget;
    /** Per-model pricing override for cost accounting (defaults to DEFAULT_PRICING). */
    pricing?: Record<string, ModelPricing>;
    /** Optional store for persisting/resuming the conversation. Used with `conversationId`. */
    checkpointStore?: CheckpointStore;
    /** Conversation id for the checkpoint store (required to use checkpointing). */
    conversationId?: string;
    /**
     * Optional tool-access policy. When set, a tool the clearance forbids is not
     * dispatched — a "tool not permitted" result is returned to the model instead.
     * Undefined allows every tool (the prior behaviour).
     */
    clearance?: Clearance;
    /**
     * Optional human-in-the-loop gate. When set, the agent asks it for approval before
     * running any tool call for which {@link requiresApproval} returns true. A denied call
     * is not executed; the model is told it was denied and can adapt.
     */
    humanGate?: HumanGate;
    /**
     * Which tool calls need human approval (e.g. writes / destructive actions), given the
     * tool name and parsed arguments. Default: none. Only consulted when `humanGate` is set.
     * Example: `requiresApproval: (name) => name === 'delete_record' || name === 'send_email'`.
     */
    requiresApproval?: (name: string, args: Record<string, unknown>) => boolean;
    /**
     * Number of ADDITIONAL attempts after the first if the model call throws a transient
     * error (rate-limit, 5xx, dropped connection). `0` (the default) preserves today's
     * behaviour: a single attempt, error propagates immediately. Only the model call is
     * retried — never tool execution.
     */
    maxRetries?: number;
    /**
     * Base delay (milliseconds) for exponential backoff between retries. The wait before
     * retry attempt `n` (1-indexed) is `retryBackoffMs * 2 ** (n - 1)`. Defaults to 200.
     * Set to `0` to retry without sleeping (used by tests).
     */
    retryBackoffMs?: number;
}

export interface AgentRunResponse {
    text: string;
    iterations: number;
    toolCalls: number;
    usage: Usage;
    costUsd: number;
    /** True if the turn stopped because the cost/token budget was hit. */
    budgetExceeded: boolean;
}

/**
 * One streamed chunk from a streaming chat completion — the standard OpenAI
 * `chat.completions` streaming chunk shape. `content` deltas concatenate into the
 * assistant text; `tool_calls` fragments are assembled by their `index` (the `id`
 * + `function.name` appear when the call first opens, `function.arguments` arrives
 * in fragments). `usage` is sent by gateways on (typically) the final chunk.
 */
export interface ChatChunk {
    choices: Array<{
        delta: {
            content?: string | null;
            tool_calls?: Array<{
                index: number;
                id?: string;
                function?: { name?: string; arguments?: string };
            }> | null;
        };
    }>;
    usage?: { prompt_tokens?: number | null; completion_tokens?: number | null } | null;
}

/**
 * The minimal shape of the OpenAI-compatible client the agent needs. The real
 * `openai` SDK's `OpenAI` satisfies this; tests inject a fake.
 *
 * `chat.completions.create` is the non-streaming call the {@link SmoothAgent.run}
 * loop uses. `createStream` is the optional streaming call the
 * {@link SmoothAgent.runStream} loop uses — production wires it to the real SDK's
 * `create({ ...body, stream: true })` (which returns an async-iterable of
 * {@link ChatChunk}s). It is optional so non-streaming consumers and the existing
 * fakes keep satisfying the interface; `runStream` throws if it is absent.
 */
export interface ChatClientLike {
    chat: {
        completions: {
            create(body: Record<string, unknown>): Promise<{
                choices: Array<{
                    message: {
                        content: string | null;
                        tool_calls?: Array<{ id: string; function: { name: string; arguments: string } }> | null;
                    };
                }>;
                usage?: { prompt_tokens?: number | null; completion_tokens?: number | null } | null;
            }>;
            /**
             * Streaming variant of {@link create}. Production wires this to the real
             * `openai` SDK's `create({ ...body, stream: true })`, which returns an
             * `AsyncIterable<ChatChunk>`. Optional so non-streaming clients still satisfy
             * the seam; {@link SmoothAgent.runStream} requires it.
             */
            createStream?(body: Record<string, unknown>): AsyncIterable<ChatChunk>;
        };
    };
}

/**
 * A streamed event from {@link SmoothAgent.runStream}. A tagged union discriminated
 * on `type`, mirroring the C# `RunStreamingAsync` update sequence and the Rust
 * reference engine's event stream:
 *
 * - `text`     — an incremental assistant content delta as it streams in.
 * - `tool_call`— a tool call the model requested, emitted once (after the model
 *                stream for the iteration completes) before it is dispatched.
 * - `tool_result` — a tool's result, emitted after it finishes.
 * - `done`     — the single terminal event, carrying the same {@link AgentRunResponse}
 *                that {@link SmoothAgent.run} would return for the same script.
 */
export type StreamEvent =
    | { type: 'text'; text: string }
    | { type: 'tool_call'; name: string; arguments: string }
    | { type: 'tool_result'; name: string; result: string }
    | { type: 'done'; response: AgentRunResponse };

/** An assistant message assembled from streamed {@link ChatChunk} deltas. */
interface AssembledMessage {
    content: string;
    toolCalls: Array<{ id: string; function: { name: string; arguments: string } }>;
    usage: { prompt_tokens?: number | null; completion_tokens?: number | null } | null;
}

const DEFAULTS = {
    model: 'claude-haiku-4-5',
    maxIterations: 8,
    maxTokens: 512,
    temperature: 0,
    knowledgeTopK: 4,
    maxContextTokens: 8000,
    maxRetries: 0,
    retryBackoffMs: 200,
};

/** Sleep for `ms` milliseconds; a no-op when `ms <= 0` (so tests don't actually wait). */
function sleep(ms: number): Promise<void> {
    return ms > 0 ? new Promise((resolve) => setTimeout(resolve, ms)) : Promise.resolve();
}

/** Pull token usage from an OpenAI-shaped response, defaulting to zero when absent. */
function extractUsage(usage: { prompt_tokens?: number | null; completion_tokens?: number | null } | null | undefined): Usage {
    return { promptTokens: usage?.prompt_tokens ?? 0, completionTokens: usage?.completion_tokens ?? 0 };
}

export class SmoothAgent {
    private readonly toolsByName: Map<string, Tool>;

    constructor(
        private readonly client: ChatClientLike,
        private readonly options: AgentOptions = {},
    ) {
        if (!client) throw new Error('client is required');
        this.toolsByName = new Map((options.tools ?? []).map((t) => [t.name, t]));
    }

    private buildSystem(message: string): string {
        let system = this.options.instructions ?? '';

        const mem = this.options.memory;
        if (mem) {
            const recalled = mem.recall(message, this.options.memoryTopK ?? 4);
            if (recalled.length > 0) {
                const block = recalled.map((e) => `- ${e.text}`).join('\n');
                system = `${system}\n\nRelevant memory (things you remember about this user/context):\n${block}`.trim();
            }
        }

        const kb = this.options.knowledge;
        if (kb) {
            const topK = this.options.knowledgeTopK ?? DEFAULTS.knowledgeTopK;
            const candidateK = Math.max(this.options.knowledgeCandidateK ?? 0, topK);
            let hits = kb.query(message, candidateK);
            if (this.options.reranker) hits = this.options.reranker.rerank(message, hits);
            hits = hits.slice(0, topK);
            if (hits.length > 0) {
                const block = hits.map((h) => `[${h.source}] ${h.content}`).join('\n\n');
                system = `${system}\n\nKnowledge base (ground all facts ONLY in this; if it is not here, say you don't know):\n${block}`.trim();
            }
        }
        return system;
    }

    private toolSpecs(search?: ToolSearch): Array<Record<string, unknown>> | undefined {
        // Eager (always-visible) tools, plus — when deferred tools exist — the
        // built-in `tool_search` meta-tool and any deferred tools promoted so far
        // this run. Deferred-but-unpromoted tools are deliberately omitted so the
        // model never sees their schemas until it searches for them.
        const visible: Tool[] = [...(this.options.tools ?? [])];
        if (search?.hasDeferred()) {
            visible.push(search);
            visible.push(...search.promotedTools());
        }
        if (visible.length === 0) return undefined;
        return visible.map((t) => ({
            type: 'function',
            function: { name: t.name, description: t.description, parameters: t.parameters },
        }));
    }

    /**
     * Run a single turn.
     *
     * `history` is prior OpenAI-format messages (multi-turn). `thread`, when given,
     * is a {@link SmoothAgentThread} carrying the conversation across runs: the turn
     * is seeded from the thread's messages, and this turn's new user + assistant
     * (+ tool) messages are appended back to it before returning. The thread takes
     * precedence over `history` as the prior context.
     */
    async run(message: string, history?: Array<Record<string, unknown>>, thread?: SmoothAgentThread): Promise<AgentRunResponse> {
        const messages: Array<Record<string, unknown>> = [];
        const system = this.buildSystem(message);
        if (system) messages.push({ role: 'system', content: system });

        // Source prior conversation: the thread (if passed) wins, then the checkpoint
        // store (if configured), then the explicit `history` argument.
        const cpStore = this.options.checkpointStore;
        const cpId = this.options.conversationId;
        let prior = history;
        if (cpStore && cpId) {
            const loaded = cpStore.load(cpId);
            if (loaded) prior = loaded.messages;
        }
        if (thread) prior = [...thread.messages];
        if (prior) messages.push(...prior);
        const userMsg: Record<string, unknown> = { role: 'user', content: message };
        messages.push(userMsg);

        // Track this turn's new messages by identity so they can be appended back to
        // the thread on exit. Index slicing would be unsafe — compaction may drop or
        // reorder `messages` mid-turn.
        const turnMessages: Array<Record<string, unknown>> = [userMsg];

        // Per-run promotion state for deferred tools (undefined when none registered).
        const search = this.options.deferredTools && this.options.deferredTools.length > 0 ? new ToolSearch(this.options.deferredTools) : undefined;
        const maxIterations = this.options.maxIterations ?? DEFAULTS.maxIterations;
        let toolCalls = 0;
        let lastText = '';

        const maxContextTokens = this.options.maxContextTokens ?? DEFAULTS.maxContextTokens;
        const model = this.options.model ?? DEFAULTS.model;
        const tracker = new CostTracker();
        try {
            for (let iteration = 1; iteration <= maxIterations; iteration++) {
                // Keep the context window within budget before each model call.
                messages.splice(0, messages.length, ...compact(messages, maxContextTokens));
                // Recompute tool specs each iteration: a `tool_search` call in the
                // previous iteration may have promoted deferred tools into view.
                const tools = this.toolSpecs(search);
                const response = await this.callModel({
                    model,
                    messages,
                    ...(tools ? { tools } : {}),
                    temperature: this.options.temperature ?? DEFAULTS.temperature,
                    max_tokens: this.options.maxTokens ?? DEFAULTS.maxTokens,
                });
                tracker.record(model, extractUsage(response.usage), this.options.pricing);
                const choice = response.choices[0].message;
                lastText = choice.content ?? '';
    
                const assistantMsg: Record<string, unknown> = { role: 'assistant', content: choice.content ?? '' };
                if (choice.tool_calls && choice.tool_calls.length > 0) {
                    assistantMsg.tool_calls = choice.tool_calls.map((tc) => ({
                        id: tc.id,
                        type: 'function',
                        function: { name: tc.function.name, arguments: tc.function.arguments },
                    }));
                }
                messages.push(assistantMsg);
                turnMessages.push(assistantMsg);

                // Stop early if this turn has hit its token/cost budget.
                if (tracker.exceeds(this.options.budget)) {
                    return { text: lastText, iterations: iteration, toolCalls, usage: tracker.usage, costUsd: tracker.costUsd, budgetExceeded: true };
                }
    
                if (!choice.tool_calls || choice.tool_calls.length === 0) {
                    return { text: lastText, iterations: iteration, toolCalls, usage: tracker.usage, costUsd: tracker.costUsd, budgetExceeded: false };
                }
    
                toolCalls += choice.tool_calls.length;
                // Dispatch the tool calls — concurrently when enabled and there's more than
                // one — but always append the results in the original tool_calls order so the
                // transcript stays deterministic. dispatchTool turns failures/denials into a
                // result string, so Promise.all never rejects and cancels its siblings.
                const calls = choice.tool_calls;
                let results: string[];
                if (this.options.parallelToolCalls && calls.length > 1) {
                    results = await Promise.all(calls.map((tc) => this.dispatchTool(tc.function.name, tc.function.arguments, search)));
                } else {
                    results = [];
                    for (const tc of calls) {
                        results.push(await this.dispatchTool(tc.function.name, tc.function.arguments, search));
                    }
                }
                for (let i = 0; i < calls.length; i++) {
                    const toolMsg: Record<string, unknown> = { role: 'tool', tool_call_id: calls[i].id, content: results[i] };
                    messages.push(toolMsg);
                    turnMessages.push(toolMsg);
                }
            }

            return { text: lastText, iterations: maxIterations, toolCalls, usage: tracker.usage, costUsd: tracker.costUsd, budgetExceeded: false };
        } finally {
            // Persist the conversation (sans system prompt, which is rebuilt each turn).
            if (cpStore && cpId) {
                cpStore.save({ conversationId: cpId, messages: messages.filter((m) => m.role !== 'system') });
            }
            // Append this turn's new messages (user + assistant + tool, never system)
            // back to the thread so the next run sees the full conversation.
            if (thread) thread.extend(turnMessages);
        }
    }

    /**
     * Stream a single turn, yielding incremental {@link StreamEvent}s as the model
     * produces them. This drives the SAME agentic loop as {@link run} (system /
     * knowledge / memory build, seed messages, per-iteration compaction, cost
     * tracking, budget early-stop, deferred-tool specs, clearance + human-gate on
     * dispatch, checkpoint/thread persistence on exit) — but calls the model in
     * STREAMING mode and emits events as work happens:
     *
     * - a `text` event per non-empty content delta as it streams in;
     * - a `tool_call` event per requested tool call, after that iteration's model
     *   stream ends, BEFORE the call is dispatched;
     * - a `tool_result` event per tool, after it finishes (in original call order
     *   even when `parallelToolCalls` runs them concurrently);
     * - exactly one terminal `done` event carrying the same {@link AgentRunResponse}
     *   {@link run} would return for the same script.
     *
     * NOTE: retry-with-backoff (`maxRetries`/`retryBackoffMs`) is intentionally NOT
     * applied here — re-running the call after a mid-stream failure would re-emit
     * already-yielded chunks. Retry stays scoped to non-streaming {@link run}; this
     * mirrors the C# `RunStreamingAsync` decision.
     */
    async *runStream(message: string, history?: Array<Record<string, unknown>>, thread?: SmoothAgentThread): AsyncGenerator<StreamEvent> {
        const createStream = this.client.chat.completions.createStream?.bind(this.client.chat.completions);
        if (!createStream) throw new Error('runStream requires a streaming-capable client (chat.completions.createStream)');

        const messages: Array<Record<string, unknown>> = [];
        const system = this.buildSystem(message);
        if (system) messages.push({ role: 'system', content: system });

        // Source prior conversation: the thread (if passed) wins, then the checkpoint
        // store (if configured), then the explicit `history` argument. (Same as `run`.)
        const cpStore = this.options.checkpointStore;
        const cpId = this.options.conversationId;
        let prior = history;
        if (cpStore && cpId) {
            const loaded = cpStore.load(cpId);
            if (loaded) prior = loaded.messages;
        }
        if (thread) prior = [...thread.messages];
        if (prior) messages.push(...prior);
        const userMsg: Record<string, unknown> = { role: 'user', content: message };
        messages.push(userMsg);

        const turnMessages: Array<Record<string, unknown>> = [userMsg];
        const search = this.options.deferredTools && this.options.deferredTools.length > 0 ? new ToolSearch(this.options.deferredTools) : undefined;
        const maxIterations = this.options.maxIterations ?? DEFAULTS.maxIterations;
        let toolCalls = 0;
        let lastText = '';

        const maxContextTokens = this.options.maxContextTokens ?? DEFAULTS.maxContextTokens;
        const model = this.options.model ?? DEFAULTS.model;
        const tracker = new CostTracker();
        try {
            for (let iteration = 1; iteration <= maxIterations; iteration++) {
                messages.splice(0, messages.length, ...compact(messages, maxContextTokens));
                const tools = this.toolSpecs(search);

                // Stream the model call, yielding text deltas as they arrive while
                // accumulating the full assistant message (content + tool calls + usage).
                const assembled: AssembledMessage = { content: '', toolCalls: [], usage: null };
                const partials = new Map<number, { id: string; name: string; arguments: string }>();
                const stream = createStream({
                    model,
                    messages,
                    ...(tools ? { tools } : {}),
                    temperature: this.options.temperature ?? DEFAULTS.temperature,
                    max_tokens: this.options.maxTokens ?? DEFAULTS.maxTokens,
                    stream: true,
                });
                for await (const chunk of stream) {
                    if (chunk.usage) assembled.usage = chunk.usage;
                    const delta = chunk.choices[0]?.delta;
                    if (!delta) continue;
                    if (delta.content) {
                        assembled.content += delta.content;
                        yield { type: 'text', text: delta.content };
                    }
                    for (const tc of delta.tool_calls ?? []) {
                        const cur = partials.get(tc.index) ?? { id: '', name: '', arguments: '' };
                        if (tc.id) cur.id = tc.id;
                        if (tc.function?.name) cur.name = tc.function.name;
                        if (tc.function?.arguments) cur.arguments += tc.function.arguments;
                        partials.set(tc.index, cur);
                    }
                }
                // Materialize accumulated tool calls in ascending index order.
                assembled.toolCalls = [...partials.entries()]
                    .sort((a, b) => a[0] - b[0])
                    .map(([, p]) => ({ id: p.id, function: { name: p.name, arguments: p.arguments } }));

                tracker.record(model, extractUsage(assembled.usage), this.options.pricing);
                lastText = assembled.content;

                const assistantMsg: Record<string, unknown> = { role: 'assistant', content: assembled.content };
                if (assembled.toolCalls.length > 0) {
                    assistantMsg.tool_calls = assembled.toolCalls.map((tc) => ({
                        id: tc.id,
                        type: 'function',
                        function: { name: tc.function.name, arguments: tc.function.arguments },
                    }));
                }
                messages.push(assistantMsg);
                turnMessages.push(assistantMsg);

                if (tracker.exceeds(this.options.budget)) {
                    yield {
                        type: 'done',
                        response: { text: lastText, iterations: iteration, toolCalls, usage: tracker.usage, costUsd: tracker.costUsd, budgetExceeded: true },
                    };
                    return;
                }

                if (assembled.toolCalls.length === 0) {
                    yield {
                        type: 'done',
                        response: { text: lastText, iterations: iteration, toolCalls, usage: tracker.usage, costUsd: tracker.costUsd, budgetExceeded: false },
                    };
                    return;
                }

                toolCalls += assembled.toolCalls.length;
                const calls = assembled.toolCalls;
                // Emit a tool_call event per requested call (original order) BEFORE dispatch.
                for (const tc of calls) {
                    yield { type: 'tool_call', name: tc.function.name, arguments: tc.function.arguments };
                }
                // Reuse the SAME dispatch path as `run` (clearance, human-gate, tool_search,
                // JSON parsing, error-to-string, parallelToolCalls). Results are surfaced in
                // original call order so the event stream stays deterministic.
                let results: string[];
                if (this.options.parallelToolCalls && calls.length > 1) {
                    results = await Promise.all(calls.map((tc) => this.dispatchTool(tc.function.name, tc.function.arguments, search)));
                } else {
                    results = [];
                    for (const tc of calls) {
                        results.push(await this.dispatchTool(tc.function.name, tc.function.arguments, search));
                    }
                }
                for (let i = 0; i < calls.length; i++) {
                    const toolMsg: Record<string, unknown> = { role: 'tool', tool_call_id: calls[i].id, content: results[i] };
                    messages.push(toolMsg);
                    turnMessages.push(toolMsg);
                    yield { type: 'tool_result', name: calls[i].function.name, result: results[i] };
                }
            }

            yield {
                type: 'done',
                response: { text: lastText, iterations: maxIterations, toolCalls, usage: tracker.usage, costUsd: tracker.costUsd, budgetExceeded: false },
            };
        } finally {
            if (cpStore && cpId) {
                cpStore.save({ conversationId: cpId, messages: messages.filter((m) => m.role !== 'system') });
            }
            if (thread) thread.extend(turnMessages);
        }
    }

    /**
     * Invoke the model with bounded retry-with-exponential-backoff.
     *
     * On a transient error (anything the client throws — rate-limit, 5xx, dropped
     * connection) the call is retried up to `maxRetries` additional times, waiting
     * `retryBackoffMs * 2 ** (n - 1)` ms before the n-th (1-indexed) retry. If all
     * attempts fail the LAST error propagates, so the turn fails exactly as it did
     * before retries existed. Only this model call is retried — tool execution is not.
     */
    private async callModel(body: Record<string, unknown>): Promise<Awaited<ReturnType<ChatClientLike['chat']['completions']['create']>>> {
        const maxRetries = this.options.maxRetries ?? DEFAULTS.maxRetries;
        const backoffMs = this.options.retryBackoffMs ?? DEFAULTS.retryBackoffMs;
        let attempt = 0;
        for (;;) {
            try {
                return await this.client.chat.completions.create(body);
            } catch (err) {
                if (attempt >= maxRetries) throw err; // retries exhausted (or disabled): propagate last error
                attempt++;
                await sleep(backoffMs * 2 ** (attempt - 1));
            }
        }
    }

    private async dispatchTool(name: string, rawArgs: string, search?: ToolSearch): Promise<string> {
        // Enforce the role's tool clearance before dispatch: a forbidden tool is
        // never executed — the model is told it isn't permitted, mirroring how the
        // loop surfaces other tool errors.
        const clearance = this.options.clearance;
        if (clearance && !clearance.isAllowed(name)) {
            return `error: tool '${name}' is not permitted for this role`;
        }

        // Resolve the tool: eager tools first, then the built-in `tool_search`
        // meta-tool, then deferred tools that have been promoted. An unpromoted
        // deferred tool resolves to nothing — it's invisible until searched for.
        let tool = this.toolsByName.get(name);
        if (!tool && search) {
            tool = name === search.name ? search : search.toolByName(name);
        }
        if (!tool) return `error: unknown tool '${name}'`;
        let args: Record<string, unknown>;
        try {
            args = rawArgs ? JSON.parse(rawArgs) : {};
        } catch {
            return `error: tool '${name}' received invalid JSON arguments`;
        }

        // Human-in-the-loop: pause for approval before running a flagged (write/sensitive)
        // tool. A denial is fed back to the model as a result — the tool never runs.
        const gate = this.options.humanGate;
        if (gate && this.options.requiresApproval?.(name, args)) {
            const decision = await gate({ toolName: name, arguments: args, prompt: `Approve calling tool '${name}'?` });
            if (!isApproved(decision)) {
                return `Denied by human: ${decision.reason ?? 'no reason given'}`;
            }
        }

        try {
            return await tool.execute(args);
        } catch (err) {
            // Surface tool failures to the model, don't crash the turn.
            return `error: tool '${name}' failed: ${err instanceof Error ? err.message : String(err)}`;
        }
    }
}

/**
 * Build a {@link Tool} that delegates a subtask to a child {@link SmoothAgent}.
 *
 * A sub-agent is just a tool backed by another agent: the model calls this tool
 * with a `task` argument, the child agent runs that task, and the child's final
 * reply becomes the tool result — composing with the existing tool loop, no special
 * wiring. The child can have its own instructions, tools, knowledge, etc.
 */
export function delegateTool(name: string, description: string, child: SmoothAgent, taskProperty = 'task'): Tool {
    return {
        name,
        description,
        parameters: {
            type: 'object',
            properties: { [taskProperty]: { type: 'string', description: 'The subtask for the sub-agent to perform.' } },
            required: [taskProperty],
        },
        async execute(args: Record<string, unknown>): Promise<string> {
            const task = String(args[taskProperty] ?? '');
            const result = await child.run(task);
            return result.text;
        },
    };
}
