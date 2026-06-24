/**
 * An `LlmProvider` seam over the LLM call so the agentic loop can be unit-tested
 * deterministically, without a live model or network.
 *
 * The agent already takes an injected OpenAI-compatible chat client
 * ({@link ChatClientLike}). This module *formalizes* that as the provider seam —
 * `LlmProvider` is an alias of `ChatClientLike`, so the existing `SmoothAgent`
 * constructor is unchanged and backward compatible (the real `openai` SDK still
 * satisfies it).
 *
 * It also ships a reusable, exported {@link MockLlmProvider} that replaces the
 * ad-hoc fake clients the tests rolled by hand. The mock:
 *
 * - is constructed with a script of responses — plain text, tool-call responses,
 *   and errors;
 * - returns them in FIFO order across calls;
 * - records each request (the messages + tool specs it was given) so a test can
 *   assert on what the agent sent.
 *
 * This mirrors the BEHAVIOR of the Rust reference's `MockLlmClient`
 * (`rust/smooth-operator-core/src/llm_provider.rs`). The mock implements both the
 * non-streaming `create` seam (used by {@link SmoothAgent.run}) and the streaming
 * `createStream` seam (used by {@link SmoothAgent.runStream}): it replays the SAME
 * FIFO script as chunked deltas — text split into a few pieces, tool-call
 * `arguments` split across two chunks to exercise the accumulator, and a final
 * chunk carrying usage. Structured-output lands when that feature lands here.
 */

import type { ChatChunk, ChatClientLike } from './agent.js';

/** The LLM call surface the agent loop depends on. Identical to {@link ChatClientLike}. */
export type LlmProvider = ChatClientLike;

/** An OpenAI-shaped assistant message — the `choices[0].message` the agent reads. */
export interface ScriptedMessage {
    content: string | null;
    tool_calls?: Array<{ id: string; function: { name: string; arguments: string } }> | null;
}

/** Build a plain-text scripted response (no tool calls). */
export function textResponse(content: string): ScriptedMessage {
    return { content };
}

/** Build a scripted response that requests a single tool call. */
export function toolCallResponse(id: string, name: string, args: string): ScriptedMessage {
    return { content: null, tool_calls: [{ id, function: { name, arguments: args } }] };
}

/** One request the mock received, captured for assertions. */
export interface RecordedCall {
    /** The full request body passed to `chat.completions.create`. */
    body: Record<string, unknown>;
    /** The messages passed on this call. */
    messages: Array<Record<string, unknown>>;
    /** The tool specs offered to the model, if any. */
    tools?: Array<Record<string, unknown>>;
}

/** Optional token usage to attach to a scripted response (the model/gateway reports it). */
export interface ScriptedUsage {
    prompt_tokens?: number;
    completion_tokens?: number;
}

/** A scripted outcome: either a response message (with optional usage) or an error to throw. */
type Outcome = { kind: 'message'; message: ScriptedMessage; usage?: ScriptedUsage } | { kind: 'error'; message: string };

/**
 * A deterministic {@link LlmProvider} for tests. Script the responses it should
 * return (FIFO), drive your code, then assert on {@link MockLlmProvider.calls}.
 *
 * Construct empty and build up fluently (`pushText` / `pushToolCall` / `pushError`),
 * or pass an initial script of {@link ScriptedMessage}s.
 *
 * @example
 * const mock = new MockLlmProvider();
 * mock.pushText('hello there');
 * const agent = new SmoothAgent(mock, {});
 * const result = await agent.run('hi');
 * expect(result.text).toBe('hello there');
 * expect(mock.callCount).toBe(1);
 */
export class MockLlmProvider implements ChatClientLike {
    private readonly script: Outcome[] = [];
    private readonly recorded: RecordedCall[] = [];

    constructor(script: ScriptedMessage[] = []) {
        this.script = script.map((message) => ({ kind: 'message', message }));
    }

    // ── scripting (fluent: each returns this) ────────────────────────────────

    /** Queue a raw OpenAI-shaped assistant message (with optional usage) for the next call. */
    pushResponse(message: ScriptedMessage, usage?: ScriptedUsage): this {
        this.script.push({ kind: 'message', message, usage });
        return this;
    }

    /** Queue a plain-text response (with optional usage) for the next call. */
    pushText(content: string, usage?: ScriptedUsage): this {
        return this.pushResponse(textResponse(content), usage);
    }

    /** Queue a single-tool-call response (with optional usage) for the next call. */
    pushToolCall(id: string, name: string, args: string, usage?: ScriptedUsage): this {
        return this.pushResponse(toolCallResponse(id, name, args), usage);
    }

    /** Queue an error to be thrown on the next call. */
    pushError(message: string): this {
        this.script.push({ kind: 'error', message });
        return this;
    }

    // ── recordings ───────────────────────────────────────────────────────────

    /** Every request the mock has received so far, in order. */
    get calls(): readonly RecordedCall[] {
        return this.recorded;
    }

    /** Number of requests received. */
    get callCount(): number {
        return this.recorded.length;
    }

    /** The most recent request, if any. */
    get lastCall(): RecordedCall | undefined {
        return this.recorded[this.recorded.length - 1];
    }

    // ── the ChatClientLike surface ───────────────────────────────────────────

    private record(body: Record<string, unknown>): void {
        this.recorded.push({
            body,
            messages: (body.messages as Array<Record<string, unknown>>) ?? [],
            tools: body.tools as Array<Record<string, unknown>> | undefined,
        });
    }

    readonly chat = {
        completions: {
            create: async (body: Record<string, unknown>) => {
                this.record(body);
                const outcome = this.script.shift();
                if (outcome?.kind === 'error') throw new Error(outcome.message);
                // Empty script: a benign terminal text response so loops don't hang.
                const message: ScriptedMessage = outcome?.message ?? { content: '' };
                const usage = outcome?.kind === 'message' ? outcome.usage : undefined;
                return { choices: [{ message }], usage: usage ?? null };
            },

            // Streaming seam: replays the SAME FIFO script as chunked deltas. Text is
            // split into a few pieces (so consumers see multiple `text` events); a
            // tool call's `arguments` is split across two chunks (so the agent's
            // accumulator is exercised); a final empty-delta chunk carries usage.
            createStream: (body: Record<string, unknown>): AsyncIterable<ChatChunk> => {
                this.record(body);
                const outcome = this.script.shift();
                const message: ScriptedMessage = outcome?.kind === 'message' ? outcome.message : { content: '' };
                const usage = outcome?.kind === 'message' ? outcome.usage : undefined;
                const error = outcome?.kind === 'error' ? outcome.message : undefined;

                async function* gen(): AsyncGenerator<ChatChunk> {
                    if (error) throw new Error(error);
                    // Text content → 2-3 deltas.
                    const content = message.content ?? '';
                    for (const piece of splitIntoChunks(content, 3)) {
                        if (piece) yield { choices: [{ delta: { content: piece } }] };
                    }
                    // Tool calls → opening chunk (id + name + first arg half), then a
                    // second chunk with the rest of the arguments. Exercises the
                    // index-keyed accumulator on the agent side.
                    for (const [index, tc] of (message.tool_calls ?? []).entries()) {
                        const args = tc.function.arguments ?? '';
                        const mid = Math.floor(args.length / 2);
                        yield {
                            choices: [{ delta: { tool_calls: [{ index, id: tc.id, function: { name: tc.function.name, arguments: args.slice(0, mid) } }] } }],
                        };
                        yield {
                            choices: [{ delta: { tool_calls: [{ index, function: { arguments: args.slice(mid) } }] } }],
                        };
                    }
                    // Final chunk carries usage (gateways send it on the last chunk).
                    yield { choices: [{ delta: {} }], usage: usage ? { prompt_tokens: usage.prompt_tokens ?? 0, completion_tokens: usage.completion_tokens ?? 0 } : null };
                }
                return gen();
            },
        },
    };
}

/** Split `s` into up to `n` roughly-equal non-empty pieces (≥2 when long enough). */
function splitIntoChunks(s: string, n: number): string[] {
    if (s.length === 0) return [];
    const parts = Math.min(n, Math.max(1, s.length));
    const size = Math.ceil(s.length / parts);
    const out: string[] = [];
    for (let i = 0; i < s.length; i += size) out.push(s.slice(i, i + size));
    return out;
}
