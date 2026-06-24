/**
 * In-memory knowledge base for the TypeScript smooth-operator core.
 *
 * A minimal lexical-overlap retriever — the Phase-0 sibling of the Rust engine's
 * in-memory lexical store and the Python core's `InMemoryKnowledge`. Documents
 * are scored by token overlap with the query; the top-k are returned. When
 * nothing overlaps, the first k documents are returned anyway so the agent still
 * has context to ground (or honestly decline) against.
 */

export interface KnowledgeHit {
    content: string;
    source: string;
    score: number;
}

/**
 * A retriever: returns the most relevant documents for a query. Both the lexical
 * {@link InMemoryKnowledge} and the embedding-backed `VectorKnowledge` satisfy
 * this, so the agent accepts either.
 */
export interface Knowledge {
    query(query: string, topK?: number): KnowledgeHit[];
}

interface Doc {
    content: string;
    source: string;
}

function tokenize(text: string): string[] {
    return text.toLowerCase().match(/[a-z0-9]+/g) ?? [];
}

export class InMemoryKnowledge {
    private readonly docs: Doc[] = [];

    /** Add a document to the knowledge base. */
    ingest(content: string, source: string): void {
        this.docs.push({ content, source });
    }

    /** Return up to `topK` documents, ranked by token overlap with `query`. */
    query(query: string, topK = 4): KnowledgeHit[] {
        const qTokens = new Set(tokenize(query));
        const scored = this.docs.map((doc) => {
            const docTokens = new Set(tokenize(doc.content));
            let overlap = 0;
            for (const t of docTokens) if (qTokens.has(t)) overlap++;
            return { overlap, doc };
        });
        scored.sort((a, b) => b.overlap - a.overlap);

        const hits = scored
            .slice(0, topK)
            .filter((s) => s.overlap > 0)
            .map((s) => ({ content: s.doc.content, source: s.doc.source, score: s.overlap }));

        if (hits.length === 0) {
            // No lexical overlap — still hand the model the first k docs so it can
            // ground or honestly decline, rather than retrieving nothing.
            return this.docs.slice(0, topK).map((doc) => ({ content: doc.content, source: doc.source, score: 0 }));
        }
        return hits;
    }
}
