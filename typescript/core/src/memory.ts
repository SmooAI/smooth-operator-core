/**
 * Long-term memory — facts the agent carries across conversations.
 *
 * Phase-1 sibling of the reference engines' memory. Distinct from checkpointing
 * (which persists a single conversation's messages): `Memory` is a durable pool of
 * standalone facts the agent recalls into context on any turn, keyed by relevance
 * to the current message. `InMemoryMemory` is the zero-dependency default (lexical
 * recall); a vector-backed memory drops in behind the interface.
 */

function tokens(text: string): string[] {
    return text.toLowerCase().match(/[a-z0-9]+/g) ?? [];
}

export interface MemoryEntry {
    text: string;
}

export interface Memory {
    remember(text: string): void;
    recall(query: string, topK?: number): MemoryEntry[];
}

/** A process-local memory pool with lexical-overlap recall. */
export class InMemoryMemory implements Memory {
    private readonly entries: MemoryEntry[] = [];

    remember(text: string): void {
        const t = text.trim();
        if (t) this.entries.push({ text: t });
    }

    recall(query: string, topK = 4): MemoryEntry[] {
        if (topK <= 0) return [];
        const qTerms = new Set(tokens(query));
        return this.entries
            .map((e, i) => {
                const docTerms = new Set(tokens(e.text));
                let overlap = 0;
                for (const t of qTerms) if (docTerms.has(t)) overlap++;
                return { e, i, overlap };
            })
            .filter((x) => x.overlap > 0)
            .sort((a, b) => b.overlap - a.overlap || a.i - b.i)
            .slice(0, topK)
            .map((x) => x.e);
    }
}
