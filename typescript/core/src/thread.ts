/**
 * Conversation threads — carry message history across `SmoothAgent.run` calls.
 *
 * Phase-2 sibling of the C# `SmoothAgentThread` (`dotnet/core`) and the reference
 * engine's persisted `Conversation`. A {@link SmoothAgentThread} is the in-memory
 * handle you hold per user conversation and pass to each run: the agent seeds the
 * turn from the thread's messages, runs, and appends this turn's user/assistant/tool
 * messages back to it, so the next turn has the full context. The system prompt is
 * supplied per-run from instructions/knowledge/memory and is *never* stored here.
 *
 * This complements checkpointing (`./checkpoint.ts`): a checkpoint *persists* a
 * conversation to a store keyed by id; a thread is the live in-memory object you
 * pass between runs. The thread's {@link SmoothAgentThread.id} is the natural key
 * to checkpoint under.
 */

type Message = Record<string, unknown>;

/** Generate a random hex id (crypto.randomUUID without the dashes). */
function newThreadId(): string {
    return globalThis.crypto.randomUUID().replace(/-/g, '');
}

/**
 * A conversation thread: a stable id plus the ordered non-system messages so far.
 *
 * Construct fresh (a new id is generated) or pass an `id` to resume one (e.g. a key
 * recovered from a checkpoint):
 *
 * ```ts
 * const thread = new SmoothAgentThread();              // fresh conversation
 * const resumed = new SmoothAgentThread('conv-42');    // resume by id
 * ```
 */
export class SmoothAgentThread {
    readonly id: string;
    /** Ordered history, oldest first, never including a system message. */
    private readonly _messages: Message[] = [];

    constructor(id?: string, messages?: Message[]) {
        this.id = id && id.length > 0 ? id : newThreadId();
        if (messages) this.extend(messages);
    }

    /** The accumulated history, oldest first (no system prompt). */
    get messages(): readonly Message[] {
        return this._messages;
    }

    /** Number of messages currently in the thread. */
    get length(): number {
        return this._messages.length;
    }

    /** Append one message, skipping any system message (rebuilt per-run). */
    add(message: Message): void {
        if (message.role === 'system') return;
        this._messages.push(message);
    }

    /** Append several messages, skipping any system messages. */
    extend(messages: readonly Message[]): void {
        for (const m of messages) this.add(m);
    }
}
