/**
 * Knowledge reranking — reorder retrieved documents by relevance.
 *
 * Phase-1 sibling of the reference engines' reranking. A `Reranker` takes the
 * query and the retriever's candidate hits and returns them reordered (and
 * possibly trimmed). `NoopReranker` is the default passthrough; `LexicalReranker`
 * re-scores by query-term coverage normalized for document length, so a concise
 * on-topic doc outranks a long one with the same raw overlap. A cross-encoder or
 * gateway reranker drops in behind the same interface.
 */

import type { KnowledgeHit } from './knowledge.js';

function tokens(text: string): string[] {
    return text.toLowerCase().match(/[a-z0-9]+/g) ?? [];
}

export interface Reranker {
    rerank(query: string, hits: KnowledgeHit[]): KnowledgeHit[];
}

/** Returns the hits unchanged — the zero-cost default. */
export class NoopReranker implements Reranker {
    rerank(_query: string, hits: KnowledgeHit[]): KnowledgeHit[] {
        return hits;
    }
}

/**
 * Reorders by query-term coverage normalized by document length:
 * `coverage / log2(2 + docTokenCount)`, so coverage is rewarded but long docs are
 * penalized relative to concise ones with the same coverage. Stable for ties.
 */
export class LexicalReranker implements Reranker {
    rerank(query: string, hits: KnowledgeHit[]): KnowledgeHit[] {
        const qTerms = new Set(tokens(query));
        if (qTerms.size === 0) return hits;

        const score = (hit: KnowledgeHit): number => {
            const docTokens = tokens(hit.content);
            let coverage = 0;
            for (const t of new Set(docTokens)) if (qTerms.has(t)) coverage++;
            return coverage / Math.log2(2 + docTokens.length);
        };

        // Stable sort by descending score (decorate with index to preserve ties).
        return hits
            .map((hit, i) => ({ hit, i, s: score(hit) }))
            .sort((a, b) => b.s - a.s || a.i - b.i)
            .map((x) => x.hit);
    }
}
