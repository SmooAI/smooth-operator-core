/**
 * Conversation checkpointing — persist a turn's state so it can resume.
 *
 * Phase-1 sibling of the reference engines' checkpointing. A `CheckpointStore`
 * saves and loads the conversation (the non-system messages) keyed by a
 * conversation id, so a later turn — even in a new process — can pick up where
 * the last left off. `InMemoryCheckpointStore` is the zero-dependency default.
 */

type Message = Record<string, unknown>;

export interface Checkpoint {
    conversationId: string;
    messages: Message[];
}

export interface CheckpointStore {
    save(checkpoint: Checkpoint): void;
    load(conversationId: string): Checkpoint | undefined;
}

/** A process-local checkpoint store backed by a Map. */
export class InMemoryCheckpointStore implements CheckpointStore {
    private readonly store = new Map<string, Checkpoint>();

    save(checkpoint: Checkpoint): void {
        // Copy the messages so later mutation of the live list doesn't bleed in.
        this.store.set(checkpoint.conversationId, {
            conversationId: checkpoint.conversationId,
            messages: checkpoint.messages.map((m) => ({ ...m })),
        });
    }

    load(conversationId: string): Checkpoint | undefined {
        return this.store.get(conversationId);
    }
}
