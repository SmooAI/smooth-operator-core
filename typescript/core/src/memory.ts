/**
 * Long-term memory — facts the agent carries across conversations.
 *
 * Phase-1 sibling of the reference engines' memory. Distinct from checkpointing
 * (which persists a single conversation's messages): `Memory` is a durable pool of
 * standalone facts the agent recalls into context on any turn, keyed by relevance
 * to the current message. `InMemoryMemory` is the zero-dependency default (lexical
 * recall); a vector-backed memory drops in behind the interface.
 *
 * Recall behaviour here is a **cross-language contract**, not a local choice: the
 * Rust reference (`rust/smooth-operator-core/src/memory.rs`) and the C#, Python and
 * Go siblings all produce the same block for the same memories. Pearl th-ffaeae
 * exists because they had drifted — three spellings of the header alone, plus
 * different scores, entry formats and top-k defaults, which made a shared
 * conformance scenario impossible to write.
 */

/** Memories auto-recalled per turn when the caller does not set one. */
export const MEMORY_TOP_K = 5;

/** Header that opens every auto-recall block, in every core. */
export const RECALL_HEADER = '[Recalled memories]';

/**
 * The verify-before-recommend note (rule D6), emitted only when at least one
 * recalled entry is time-sensitive (see {@link needsFreshnessCheck}).
 *
 * A memory naming a function, file, or flag is a claim about the PAST, not a fact
 * about now — without this line the model happily recommends a symbol that was
 * deleted three releases ago.
 */
export const RECALL_FRESHNESS_NOTE =
    "Note: 'the memory says X exists' is not the same as 'X exists now'. " +
    'Before recommending or acting on any function path, file, flag, or external ' +
    "pointer named below, verify it's current by reading the file or grepping the " +
    'codebase. Project and Reference memories are time-sensitive; User and Feedback ' +
    'are durable.';

/**
 * What kind of fact a memory holds. The name is rendered into the recall block, so
 * these spellings are part of the cross-language contract.
 */
export type MemoryType = 'ShortTerm' | 'LongTerm' | 'Entity' | 'User' | 'Feedback' | 'Project' | 'Reference';

/**
 * Whether this kind of memory can go stale. `Project` and `Reference` entries name
 * things in a moving codebase or an external system; `User` and `Feedback` describe
 * the person, which does not change when someone renames a file.
 */
export function needsFreshnessCheck(type: MemoryType): boolean {
    return type === 'Project' || type === 'Reference';
}

/** Lowercased alphanumeric tokens; punctuation is a separator, not part of a token. */
function tokens(text: string): string[] {
    return text.toLowerCase().match(/[a-z0-9]+/g) ?? [];
}

export interface MemoryEntry {
    text: string;
    /** Defaults to `LongTerm` when a store does not classify its entries. */
    memoryType?: MemoryType;
    /** Populated by `recall`; rendered into the block. */
    relevance?: number;
}

export interface Memory {
    remember(text: string, memoryType?: MemoryType): void;
    recall(query: string, topK?: number): MemoryEntry[];
}

/**
 * Fraction of the QUERY's distinct tokens that appear in `content`.
 *
 * Normalised to 0–1 deliberately: a raw overlap count is comparable neither between
 * entries nor between languages, and it is rendered into the prompt. Punctuation is
 * a token separator — the Rust reference used to split on whitespace alone, so a
 * query ending "…my name?" never matched a memory containing "name" and the entry
 * was silently not recalled.
 */
export function relevanceScore(query: string, content: string): number {
    const queryTokens = new Set(tokens(query));
    if (queryTokens.size === 0) return 0;
    const contentTokens = new Set(tokens(content));
    let matching = 0;
    for (const t of queryTokens) if (contentTokens.has(t)) matching++;
    return matching / queryTokens.size;
}

/**
 * Render recalled entries as the context block injected into a turn. Byte-identical
 * to the Rust reference's `render_recall_block`. `undefined` for an empty list, so a
 * caller injects nothing rather than a bare header telling the model it remembered
 * nothing.
 */
export function renderRecallBlock(entries: readonly MemoryEntry[]): string | undefined {
    if (entries.length === 0) return undefined;
    const lines = [RECALL_HEADER];
    if (entries.some((e) => needsFreshnessCheck(e.memoryType ?? 'LongTerm'))) {
        lines.push(RECALL_FRESHNESS_NOTE);
    }
    for (const e of entries) {
        lines.push(`- (${e.memoryType ?? 'LongTerm'}, relevance=${(e.relevance ?? 0).toFixed(2)}): ${e.text}`);
    }
    return `${lines.join('\n')}\n`;
}

/** A process-local memory pool with lexical-overlap recall. */
export class InMemoryMemory implements Memory {
    private readonly entries: MemoryEntry[] = [];

    remember(text: string, memoryType: MemoryType = 'LongTerm'): void {
        const t = text.trim();
        if (t) this.entries.push({ text: t, memoryType });
    }

    recall(query: string, topK = MEMORY_TOP_K): MemoryEntry[] {
        if (topK <= 0) return [];
        return this.entries
            .map((e, i) => ({ e, i, relevance: relevanceScore(query, e.text) }))
            .filter((x) => x.relevance > 0)
            // Best first; ties keep insertion order — the sibling cores rely on that
            // to produce the same block.
            .sort((a, b) => b.relevance - a.relevance || a.i - b.i)
            .slice(0, topK)
            .map((x) => ({ ...x.e, relevance: x.relevance }));
    }
}
