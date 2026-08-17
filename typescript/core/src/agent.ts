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
import { applyCacheControl, supportsAnthropicCacheControl } from './cacheControl.js';
import { CostTracker, parseGatewayCost } from './cost.js';
import type { CostBudget, HeaderLike, ModelPricing, Usage } from './cost.js';
import type { HumanGate } from './humanGate.js';
import { isApproved } from './humanGate.js';
import type { Knowledge } from './knowledge.js';
import type { DenyPolicy } from './denyPolicy.js';
import { AutoMode, PermissionHook } from './permission.js';
import type { PermissionGrants } from './permissionGrants.js';
import { ToolSearch } from './toolSearch.js';
import { userContent } from './multimodal.js';
import type { ImageContent } from './multimodal.js';

/** A callable tool the agent may invoke. Mirrors the reference engines' tool seam. */
export interface Tool {
    name: string;
    description: string;
    /** JSON Schema for the tool's arguments. */
    parameters: Record<string, unknown>;
    execute(args: Record<string, unknown>): Promise<string>;
}

/** A tool call requested by the model. Mirrors the Rust engine's `ToolCall`. */
export interface ToolCall {
    id: string;
    name: string;
    arguments: Record<string, unknown>;
}

/**
 * Result of executing a tool. Mirrors the Rust engine's `ToolResult`.
 *
 * `content` is what the model/conversation sees — a {@link ToolHook.postCall}
 * hook may rewrite it in place (the redaction seam).
 */
export interface ToolResult {
    toolCallId: string;
    content: string;
    isError: boolean;
    /** Optional structured details for UI rendering (diffs, tables, etc.). */
    details?: unknown;
}

/**
 * A hook that runs around every tool call, mirroring the Rust engine's
 * `ToolHook` trait (`pre_call` / `post_call`). Installed on the agent via
 * {@link SmoothAgent.addHook} or {@link AgentOptions.toolHooks} and run for both
 * {@link SmoothAgent.run} and {@link SmoothAgent.runStream}.
 *
 * The lifecycle is the enforcement + redaction seam Narc (and the server's
 * consumer-supplied surveillance hooks) plug into.
 */
export interface ToolHook {
    /**
     * Called before a tool executes. **Throw to block the call** (mirrors Rust's
     * `pre_call` returning `Err`) — the model is told the call was blocked and the
     * tool never runs. Optional; omit for a post-only (redaction) hook.
     */
    preCall?(call: ToolCall): Promise<void>;
    /**
     * Called after the tool executes with a **mutable** {@link ToolResult}. A hook
     * may rewrite `result.content` (e.g. redact a leaked secret) and the mutation
     * is what the model/conversation sees. A throw here is swallowed (logged, not
     * surfaced) so the redaction seam can never break the turn — mirroring Rust's
     * `post_call` whose `Err` is `tracing::warn`'d, not propagated. Optional.
     */
    postCall?(call: ToolCall, result: ToolResult): Promise<void>;
}

/**
 * The verdict of folding the SEP `tool_call` hook chain over one pending call —
 * shaped to match `FoldedHook` from `extension/host.ts`, declared structurally
 * here so the agent loop needs no import from the extension subsystem.
 */
export type ExtensionFold = { kind: 'proceed'; value: unknown } | { kind: 'blocked'; reason: string };

/**
 * The seam through which a SEP extension host participates in the agent loop —
 * the TypeScript sibling of Rust's `Agent::with_extension_host` and Go's
 * `core.ExtensionHooks`. Declared structurally (the concrete `ExtensionHost`
 * satisfies it as-is) so `agent.ts` and `extension/` stay import-cycle-free.
 */
export interface ExtensionHooks {
    /** Eager tool proxies, already namespaced `<extension>.<tool>`. */
    tools(): Tool[];
    /** Deferred tool proxies: hidden from the model until `tool_search` promotes them. */
    deferredTools(): Tool[];
    /**
     * Fold the `tool_call` hook chain over one pending call BEFORE it executes.
     * A `blocked` verdict vetoes the call; a `proceed` value may carry rewritten
     * `arguments` (rewrites are already scoped by the host's cross-tool guard).
     */
    runToolCallHook(tool: string, args: unknown): Promise<ExtensionFold>;
    /** Fire-and-forget event fan-out to subscribed extensions; never blocks the turn. */
    dispatchEvent(event: string, payload: unknown): void;
}

/** What the model sees in place of a vetoed call's result (Rust/Go parity). */
function sepBlockedResult(reason: string): string {
    return reason ? `error: blocked by extension: ${reason}` : 'error: blocked by extension';
}

export interface AgentOptions {
    instructions?: string;
    model?: string;
    maxIterations?: number;
    maxTokens?: number;
    /**
     * The active model's hard **output** ceiling (`max_output_tokens`), when known.
     * Each model call clamps `max_tokens` to `min(maxTokens, modelMaxOutput)` so a
     * budget/policy `maxTokens` (which may be tuned high) can never exceed what the
     * model can physically emit — otherwise a reasoning model burns its budget on
     * `reasoning_content` and returns empty `content`, or the upstream 400s (e.g.
     * `groq-compound` caps output at 8192). Source it from the gateway's
     * `/model/info` (`model_info.max_output_tokens`). Omitted / `undefined` / `0` ⇒
     * no clamp (graceful passthrough, zero behaviour change). Mirrors the Rust
     * engine's `LlmClient::with_model_ceiling` / `AgentConfig.model_max_output`
     * (EPIC th-1cc9fa).
     */
    modelMaxOutput?: number;
    /**
     * Opaque object forwarded verbatim as every model request's top-level
     * `metadata` field (LiteLLM records it on spend logs — e.g. an agent slug so
     * per-agent LLM spend is queryable at the gateway). Omitted / empty ⇒ the
     * field never appears on the wire, byte-identical to unset. Mirrors the Rust
     * engine's `AgentConfig.with_metadata`.
     */
    metadata?: Record<string, unknown>;
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
     * Tool-call surveillance hooks run around every tool dispatch (both `run` and
     * `runStream`). Each hook's `preCall` runs before the tool executes — a throw
     * blocks the call — and its `postCall` runs after with a mutable result it may
     * redact. Mirrors the Rust engine's `ToolRegistry` hook chain; the server's
     * `toolHooks` seam threads consumer-supplied hooks in here. Additional hooks can
     * be added post-construction via {@link SmoothAgent.addHook}.
     */
    toolHooks?: ToolHook[];
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
     * Image attachments for the CURRENT turn's user message (a multimodal turn).
     * Set by a host that received a chat turn carrying images; emitted as OpenAI
     * `image_url` content parts on that one turn. Unset (the default) leaves every
     * text-only turn byte-identical. Mirrors Rust's `AgentConfig::with_user_images`.
     */
    nextUserImages?: ImageContent[];
    /**
     * SEP extension host participating in the agent loop — the TypeScript sibling
     * of Rust's `Agent::with_extension_host` (and Go's `AgentOptions.Extensions`).
     * The host's eager tools are merged into {@link tools} as ORDINARY tools
     * (visible, dispatched, and permission-gated exactly like native tools), its
     * deferred tools into {@link deferredTools} (hidden until `tool_search`
     * promotes them), its `tool_call` hook folds over every pending call before
     * dispatch (veto or argument rewrite — already scoped by the host's cross-tool
     * guard), and turn lifecycle events fan out to subscribed extensions.
     * The concrete `ExtensionHost` satisfies this structurally — no import needed.
     * Omitted (the default) ⇒ the loop behaves exactly as before extensions existed.
     */
    extensions?: ExtensionHooks;
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
     * Enable the native permission gate ({@link PermissionHook}). When set (or when
     * {@link denyPolicy} / {@link permissionGrants} is set, defaulting to
     * {@link AutoMode.Ask}), every tool call is classified before dispatch: read-only
     * calls allow, dangerous calls (`rm -rf /`, credential paths, `curl | sh`,
     * dangerous domains, env dumps) hard-deny in EVERY mode, and mutating/unknown
     * calls `Ask`. An `Ask` is routed to {@link humanGate} when one is wired and
     * **fails closed** (blocked, surfaced to the model) otherwise. A blocked call is
     * never executed; the model is told why. Undefined ⇒ the gate is off (prior
     * behaviour). Mirrors the Rust engine's `SMOOTH_AUTO_MODE` / `PermissionHook`.
     */
    permissionMode?: AutoMode;
    /**
     * A consumer {@link DenyPolicy} (declarative deny rules + predicates). Evaluated
     * FIRST as a circuit-breaker: a match hard-denies regardless of grants or mode
     * (including {@link AutoMode.Bypass}). Setting this alone enables the gate at
     * {@link AutoMode.Ask}. Purely additive — an empty/absent policy changes nothing.
     */
    denyPolicy?: DenyPolicy;
    /**
     * In-memory allow-list consulted before prompting on an `Ask`. A matching grant
     * auto-approves silently; an `approveAlways` answer adds a grant. Setting this
     * alone enables the gate at {@link AutoMode.Ask}. The consumer owns persistence.
     */
    permissionGrants?: PermissionGrants;
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
    /**
     * The gateway's per-request cost, when the client surfaced one. It lives ONLY in
     * a response HEADER, and a streaming client that returns a bare stream has no
     * response object to read one off at all — so it captures the response, parses
     * the cost, and rides it on a chunk (the gateway client uses a leading chunk with
     * no `choices`, matching the Go engine). Absent ⇒ unmeasured, and the local
     * pricing estimate is used instead of a bogus $0.
     */
    gatewayCostUsd?: number;
    /** Raw response headers, when the client hangs them off a chunk instead of pre-parsing. */
    headers?: unknown;
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
    /**
     * The OpenAI-compatible base URL this client talks to, when it knows it.
     * Only used to gate Anthropic prompt-cache markers (see `cacheControl.ts`) —
     * a client that doesn't set it, such as {@link MockLlmProvider}, simply never
     * gets them, leaving its request bodies byte-identical.
     */
    apiBaseUrl?: string;
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
    | {
          type: 'tool_result';
          name: string;
          result: string;
          /**
           * Structured, UI-facing payload a postCall hook attached to the
           * {@link ToolResult} (undefined when none) — forwarded verbatim and
           * un-truncated, never shown to the model. Mirrors the Rust engine's
           * `AgentEvent::ToolCallComplete.details`.
           */
          details?: unknown;
      }
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

/**
 * The `max_tokens` to actually send: the configured budget, clamped down to the
 * model's output ceiling when one is known. Never returns 0. `ceiling` of
 * `undefined` / `0` (or any non-positive value) ⇒ passthrough (no clamp), mirroring
 * the Rust engine's `LlmClient::effective_max_tokens` (EPIC th-1cc9fa).
 */
/**
 * Spreadable `{ metadata }` fragment for a model request body. Empty or absent
 * metadata yields `{}` so the wire stays byte-identical when unset (Rust
 * parity: `with_metadata` filters empty maps to `None`).
 */
export function metadataField(metadata?: Record<string, unknown>): { metadata?: Record<string, unknown> } {
    return metadata && Object.keys(metadata).length > 0 ? { metadata } : {};
}

export function effectiveMaxTokens(configured: number, ceiling?: number): number {
    if (ceiling === undefined || ceiling <= 0) return configured;
    return Math.max(1, Math.min(configured, ceiling));
}

/**
 * The gateway's authoritative cost for a response, when the client surfaced one.
 *
 * The engine takes an injected OpenAI-compatible client, and the SDK's parsed
 * response carries no headers — the cost lives ONLY in a response header. So this
 * reads two shapes, in order: a `gatewayCostUsd` a wrapping client already parsed
 * and attached, or raw `headers` hanging off the response (what
 * `openai`'s `.withResponse()` gives you). Absent both, `undefined` — unmeasured,
 * and the local pricing estimate is used instead of a bogus $0.
 */
function responseGatewayCost(response: unknown): number | undefined {
    const r = response as { gatewayCostUsd?: number; headers?: unknown } | null | undefined;
    if (typeof r?.gatewayCostUsd === 'number' && r.gatewayCostUsd > 0) return r.gatewayCostUsd;
    return parseGatewayCost(r?.headers as HeaderLike | undefined);
}

/** Pull token usage from an OpenAI-shaped response, defaulting to zero when absent. */
function extractUsage(usage: { prompt_tokens?: number | null; completion_tokens?: number | null } | null | undefined): Usage {
    return { promptTokens: usage?.prompt_tokens ?? 0, completionTokens: usage?.completion_tokens ?? 0 };
}

export class SmoothAgent {
    private readonly toolsByName: Map<string, Tool>;
    /** Tool-call surveillance hooks, run in order around every dispatch. */
    private readonly hooks: ToolHook[];
    /** The native permission gate, built when permissionMode/denyPolicy/permissionGrants is set; else undefined (gate off). */
    private readonly permissionHook?: PermissionHook;

    private readonly options: AgentOptions;

    constructor(
        private readonly client: ChatClientLike,
        options: AgentOptions = {},
    ) {
        if (!client) throw new Error('client is required');
        // Merge the extension host's tools BEFORE any lookup structures are built:
        // eager tools become ordinary (visible, dispatched, permission-gated) tools;
        // deferred tools join the hidden pool. Deliberately NOT added to
        // `toolsByName` directly when deferred — an unpromoted deferred tool must
        // stay invisible until `tool_search` promotes it (Rust/Go parity).
        const ext = options.extensions;
        this.options = ext
            ? {
                  ...options,
                  tools: [...(options.tools ?? []), ...ext.tools()],
                  deferredTools: [...(options.deferredTools ?? []), ...ext.deferredTools()],
              }
            : options;
        this.toolsByName = new Map((this.options.tools ?? []).map((t) => [t.name, t]));
        this.hooks = [...(this.options.toolHooks ?? [])];
        if (this.options.permissionMode !== undefined || this.options.denyPolicy !== undefined || this.options.permissionGrants !== undefined) {
            const hook = new PermissionHook(this.options.permissionMode ?? AutoMode.Ask);
            if (this.options.humanGate) hook.withApprover(this.options.humanGate);
            if (this.options.permissionGrants) hook.withGrants(this.options.permissionGrants);
            if (this.options.denyPolicy) hook.withDenyPolicy(this.options.denyPolicy);
            this.permissionHook = hook;
        }
    }

    /** Fire-and-forget SEP event fan-out; a no-op without an extension host. */
    private sepDispatch(event: string, payload: unknown): void {
        this.options.extensions?.dispatchEvent(event, payload);
    }

    /**
     * Emit the end-of-turn SEP event pair in Rust's order: `message_end` carrying
     * the final assistant text, then `turn_end`. Called on EVERY turn exit —
     * budget-exceeded and max-iteration included — so a subscribed extension never
     * waits forever for an end event.
     */
    private sepTurnComplete(iterations: number, content: string): void {
        if (!this.options.extensions) return;
        const model = this.options.model ?? DEFAULTS.model;
        this.sepDispatch('message_end', { iteration: iterations, content });
        this.sepDispatch('turn_end', { agent_id: model, iterations });
    }

    /**
     * Fold the SEP `tool_call` hook over every pending call before any of them
     * execute — the TypeScript sibling of Rust's `sep_tool_call_plan` and Go's
     * `sepToolCallPlan`. Returns the calls to run (arguments possibly rewritten)
     * and, for any vetoed call, its id → reason. Without a host it returns the
     * input untouched. A call whose arguments do not parse as JSON skips the hook
     * so `dispatchTool` can surface its usual invalid-arguments error.
     */
    private async sepToolCallPlan(
        calls: Array<{ id: string; function: { name: string; arguments: string } }>,
    ): Promise<{ calls: Array<{ id: string; function: { name: string; arguments: string } }>; blocks?: Map<string, string> }> {
        const ext = this.options.extensions;
        if (!ext || calls.length === 0) return { calls };
        const out: typeof calls = [];
        let blocks: Map<string, string> | undefined;
        for (const tc of calls) {
            let args: unknown;
            try {
                args = tc.function.arguments ? JSON.parse(tc.function.arguments) : {};
            } catch {
                out.push(tc);
                continue;
            }
            const folded = await ext.runToolCallHook(tc.function.name, args);
            if (folded.kind === 'blocked') {
                (blocks ??= new Map()).set(tc.id, folded.reason);
                out.push(tc);
                continue;
            }
            const patched = (folded.value as { arguments?: unknown } | null | undefined)?.arguments;
            out.push(
                patched !== undefined
                    ? { ...tc, function: { ...tc.function, arguments: JSON.stringify(patched) } }
                    : tc,
            );
        }
        return { calls: out, blocks };
    }

    /**
     * Register a tool-call surveillance {@link ToolHook}, appended after any hooks
     * supplied via {@link AgentOptions.toolHooks}. Mirrors the Rust engine's
     * `ToolRegistry::add_hook`: every hook's `preCall` runs before a tool executes
     * (a throw blocks it) and its `postCall` runs after with a mutable result.
     */
    addHook(hook: ToolHook): void {
        this.hooks.push(hook);
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
        const userMsg: Record<string, unknown> = { role: 'user', content: userContent(message, this.options.nextUserImages) };
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
        this.sepDispatch('turn_start', { agent_id: model });
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
                    max_tokens: effectiveMaxTokens(this.options.maxTokens ?? DEFAULTS.maxTokens, this.options.modelMaxOutput),
                    ...metadataField(this.options.metadata),
                });
                tracker.recordWithGatewayCost(model, extractUsage(response.usage), responseGatewayCost(response), this.options.pricing);
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
                    this.sepTurnComplete(iteration, lastText);
                    return { text: lastText, iterations: iteration, toolCalls, usage: tracker.usage, costUsd: tracker.costUsd, budgetExceeded: true };
                }

                if (!choice.tool_calls || choice.tool_calls.length === 0) {
                    this.sepTurnComplete(iteration, lastText);
                    return { text: lastText, iterations: iteration, toolCalls, usage: tracker.usage, costUsd: tracker.costUsd, budgetExceeded: false };
                }

                toolCalls += choice.tool_calls.length;
                // SEP `tool_call` hook: fold over EVERY pending call before any of
                // them run, so an extension can veto or rewrite arguments. Vetoed
                // calls never reach dispatchTool; their veto reason becomes the tool
                // result so the model learns why (Rust/Go parity).
                const { calls, blocks } = await this.sepToolCallPlan(choice.tool_calls);
                // Dispatch the tool calls — concurrently when enabled and there's more than
                // one — but always append the results in the original tool_calls order so the
                // transcript stays deterministic. dispatchTool turns failures/denials into a
                // result string, so Promise.all never rejects and cancels its siblings.
                let results: string[];
                if (this.options.parallelToolCalls && calls.length > 1) {
                    results = await Promise.all(
                        calls.map((tc) => {
                            const reason = blocks?.get(tc.id);
                            if (reason !== undefined) return Promise.resolve(sepBlockedResult(reason));
                            return this.dispatchTool(tc.function.name, tc.function.arguments, search, tc.id);
                        }),
                    );
                } else {
                    results = [];
                    for (const tc of calls) {
                        const reason = blocks?.get(tc.id);
                        results.push(reason !== undefined ? sepBlockedResult(reason) : await this.dispatchTool(tc.function.name, tc.function.arguments, search, tc.id));
                    }
                }
                for (let i = 0; i < calls.length; i++) {
                    const toolMsg: Record<string, unknown> = { role: 'tool', tool_call_id: calls[i].id, content: results[i] };
                    messages.push(toolMsg);
                    turnMessages.push(toolMsg);
                }
            }

            this.sepTurnComplete(maxIterations, lastText);
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
        const userMsg: Record<string, unknown> = { role: 'user', content: userContent(message, this.options.nextUserImages) };
        messages.push(userMsg);

        const turnMessages: Array<Record<string, unknown>> = [userMsg];
        const search = this.options.deferredTools && this.options.deferredTools.length > 0 ? new ToolSearch(this.options.deferredTools) : undefined;
        const maxIterations = this.options.maxIterations ?? DEFAULTS.maxIterations;
        let toolCalls = 0;
        let lastText = '';

        const maxContextTokens = this.options.maxContextTokens ?? DEFAULTS.maxContextTokens;
        const model = this.options.model ?? DEFAULTS.model;
        this.sepDispatch('turn_start', { agent_id: model });
        const tracker = new CostTracker();
        try {
            for (let iteration = 1; iteration <= maxIterations; iteration++) {
                messages.splice(0, messages.length, ...compact(messages, maxContextTokens));
                const tools = this.toolSpecs(search);

                // Stream the model call, yielding text deltas as they arrive while
                // accumulating the full assistant message (content + tool calls + usage).
                const assembled: AssembledMessage = { content: '', toolCalls: [], usage: null };
                const partials = new Map<number, { id: string; name: string; arguments: string }>();
                const streamBody: Record<string, unknown> = {
                    model,
                    messages,
                    ...(tools ? { tools } : {}),
                    temperature: this.options.temperature ?? DEFAULTS.temperature,
                    max_tokens: effectiveMaxTokens(this.options.maxTokens ?? DEFAULTS.maxTokens, this.options.modelMaxOutput),
                    ...metadataField(this.options.metadata),
                    stream: true,
                };
                this.markPromptCache(streamBody);
                const stream = createStream(streamBody);
                // Cost lives in a response HEADER. A client that returns a bare stream
                // has no response object to read one off at all, so it captures the
                // response and rides the cost on a chunk. Mirrors python/agent.py's
                // run_stream and Go's first-chunk ChatChunk{CostUSD}.
                let gatewayCost: number | undefined;
                for await (const chunk of stream) {
                    const chunkCost = responseGatewayCost(chunk);
                    if (chunkCost !== undefined) gatewayCost = chunkCost;
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

                tracker.recordWithGatewayCost(model, extractUsage(assembled.usage), gatewayCost, this.options.pricing);
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
                    this.sepTurnComplete(iteration, lastText);
                    yield {
                        type: 'done',
                        response: { text: lastText, iterations: iteration, toolCalls, usage: tracker.usage, costUsd: tracker.costUsd, budgetExceeded: true },
                    };
                    return;
                }

                if (assembled.toolCalls.length === 0) {
                    this.sepTurnComplete(iteration, lastText);
                    yield {
                        type: 'done',
                        response: { text: lastText, iterations: iteration, toolCalls, usage: tracker.usage, costUsd: tracker.costUsd, budgetExceeded: false },
                    };
                    return;
                }

                toolCalls += assembled.toolCalls.length;
                // SEP `tool_call` hook: same fold as `run` — veto or rewrite before dispatch.
                const { calls, blocks } = await this.sepToolCallPlan(assembled.toolCalls);
                // Emit a tool_call event per requested call (original order) BEFORE dispatch.
                for (const tc of calls) {
                    yield { type: 'tool_call', name: tc.function.name, arguments: tc.function.arguments };
                }
                // Reuse the SAME dispatch path as `run` (clearance, human-gate, tool_search,
                // JSON parsing, error-to-string, parallelToolCalls). Results are surfaced in
                // original call order so the event stream stays deterministic.
                const dispatchOne = (tc: (typeof calls)[number]): Promise<ToolResult> => {
                    const reason = blocks?.get(tc.id);
                    if (reason !== undefined) return Promise.resolve({ toolCallId: tc.id, content: sepBlockedResult(reason), isError: true });
                    return this.dispatchToolResult(tc.function.name, tc.function.arguments, search, tc.id);
                };
                let results: ToolResult[];
                if (this.options.parallelToolCalls && calls.length > 1) {
                    results = await Promise.all(calls.map(dispatchOne));
                } else {
                    results = [];
                    for (const tc of calls) {
                        results.push(await dispatchOne(tc));
                    }
                }
                for (let i = 0; i < calls.length; i++) {
                    const toolMsg: Record<string, unknown> = { role: 'tool', tool_call_id: calls[i].id, content: results[i].content };
                    messages.push(toolMsg);
                    turnMessages.push(toolMsg);
                    yield { type: 'tool_result', name: calls[i].function.name, result: results[i].content, details: results[i].details };
                }
            }

            this.sepTurnComplete(maxIterations, lastText);
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
        this.markPromptCache(body);
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

    /**
     * Stamp Anthropic prompt-cache markers on an outbound body, when the upstream
     * understands them. A no-op for every other route, so the request stays
     * byte-identical on the OpenAI/Gemini/Groq paths and under the mock client.
     */
    private markPromptCache(body: Record<string, unknown>): void {
        if (supportsAnthropicCacheControl(body.model as string | undefined, this.client.apiBaseUrl)) {
            applyCacheControl(body);
        }
    }

    private async dispatchTool(name: string, rawArgs: string, search?: ToolSearch, callId = ''): Promise<string> {
        return (await this.dispatchToolResult(name, rawArgs, search, callId)).content;
    }

    /**
     * dispatchTool returning the full {@link ToolResult}, so callers that surface
     * results to a UI (runStream) can forward the structured `details` a postCall
     * hook attached — the model itself only ever sees `content`. Mirrors the Rust
     * engine's `AgentEvent::ToolCallComplete.details`.
     */
    private async dispatchToolResult(name: string, rawArgs: string, search?: ToolSearch, callId = ''): Promise<ToolResult> {
        const errResult = (content: string): ToolResult => ({ toolCallId: callId, content, isError: true });
        // Enforce the role's tool clearance before dispatch: a forbidden tool is
        // never executed — the model is told it isn't permitted, mirroring how the
        // loop surfaces other tool errors.
        const clearance = this.options.clearance;
        if (clearance && !clearance.isAllowed(name)) {
            return errResult(`error: tool '${name}' is not permitted for this role`);
        }

        // Resolve the tool: eager tools first, then the built-in `tool_search`
        // meta-tool, then deferred tools that have been promoted. An unpromoted
        // deferred tool resolves to nothing — it's invisible until searched for.
        let tool = this.toolsByName.get(name);
        if (!tool && search) {
            tool = name === search.name ? search : search.toolByName(name);
        }
        if (!tool) return errResult(`error: unknown tool '${name}'`);
        let args: Record<string, unknown>;
        try {
            args = rawArgs ? JSON.parse(rawArgs) : {};
        } catch {
            return errResult(`error: tool '${name}' received invalid JSON arguments`);
        }

        // Native permission gate: classify the call (circuit-breakers → hard deny,
        // mutating/unknown → Ask routed to the human gate, else allow). A blocked
        // call is never executed; the reason is surfaced to the model, like other
        // tool errors. The gate is only active when a permission option was set.
        if (this.permissionHook) {
            try {
                await this.permissionHook.preCall({ id: name, name, arguments: args });
            } catch (err) {
                return errResult(`error: tool '${name}' blocked by permission policy: ${err instanceof Error ? err.message : String(err)}`);
            }
        }

        // Human-in-the-loop: pause for approval before running a flagged (write/sensitive)
        // tool. A denial is fed back to the model as a result — the tool never runs.
        const gate = this.options.humanGate;
        if (gate && this.options.requiresApproval?.(name, args)) {
            const decision = await gate({ toolName: name, arguments: args, prompt: `Approve calling tool '${name}'?` });
            if (!isApproved(decision)) {
                return errResult(`Denied by human: ${decision.reason ?? 'no reason given'}`);
            }
        }

        // Tool-call surveillance lifecycle (mirrors the Rust engine's ToolHook).
        const call: ToolCall = { id: callId, name, arguments: args };

        // pre-call: any hook that throws blocks execution (fail-closed), mirroring
        // Rust's `pre_call` returning `Err`. The block reason is surfaced to the model.
        for (const hook of this.hooks) {
            if (!hook.preCall) continue;
            try {
                await hook.preCall(call);
            } catch (err) {
                return errResult(`blocked by hook: ${err instanceof Error ? err.message : String(err)}`);
            }
        }

        const result: ToolResult = { toolCallId: callId, content: '', isError: false };
        try {
            result.content = await tool.execute(args);
        } catch (err) {
            // Surface tool failures to the model, don't crash the turn.
            result.content = `error: tool '${name}' failed: ${err instanceof Error ? err.message : String(err)}`;
            result.isError = true;
        }

        // post-call: hooks may redact `result.content` in place — the mutation is
        // what the model/conversation sees. A hook throw is swallowed (never
        // surfaced) so the redaction seam can't break the turn, mirroring Rust's
        // `post_call` whose `Err` is warned, not propagated.
        for (const hook of this.hooks) {
            if (!hook.postCall) continue;
            try {
                await hook.postCall(call, result);
            } catch {
                // ponytail: swallow post-hook errors like Rust's tracing::warn — the
                // (possibly-redacted) result still reaches the caller.
            }
        }

        return result;
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
