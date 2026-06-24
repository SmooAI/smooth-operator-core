/**
 * Vector knowledge — embedding-backed semantic retrieval.
 *
 * Phase-1 sibling of the reference engines' vector store. `VectorKnowledge` embeds
 * documents and queries and retrieves by cosine similarity, satisfying the same
 * {@link Knowledge} interface as the lexical retriever (so the agent accepts
 * either). The `Embedder` is pluggable: the default `HashEmbedder` is deterministic
 * and offline (feature-hashed bag-of-words) — good for tests and a zero-dependency
 * default — while a gateway embedder drops in behind the same interface for true
 * semantics.
 */

import type { Knowledge, KnowledgeHit } from './knowledge.js';

function tokens(text: string): string[] {
    return text.toLowerCase().match(/[a-z0-9]+/g) ?? [];
}

/** A small deterministic non-cryptographic hash (FNV-1a, 32-bit). */
export function hashToken(token: string): number {
    let h = 0x811c9dc5;
    for (let i = 0; i < token.length; i++) {
        h ^= token.charCodeAt(i) & 0xff;
        h = Math.imul(h, 0x01000193) >>> 0;
    }
    return h >>> 0;
}

export interface Embedder {
    embed(text: string): number[];
}

/**
 * Deterministic, offline feature-hashing embedder. Hashes each token into one of
 * `dim` buckets (signed) and L2-normalizes. No learned semantics, but a real
 * vector with cosine geometry — docs sharing tokens land near each other.
 */
export class HashEmbedder implements Embedder {
    constructor(private readonly dim = 256) {
        if (dim <= 0) throw new Error('dim must be positive');
    }

    embed(text: string): number[] {
        const vec = new Array<number>(this.dim).fill(0);
        for (const tok of tokens(text)) {
            const h = hashToken(tok);
            const bucket = h % this.dim;
            const sign = (h >>> 31) & 1 ? -1 : 1;
            vec[bucket] += sign;
        }
        const norm = Math.sqrt(vec.reduce((s, v) => s + v * v, 0));
        return norm > 0 ? vec.map((v) => v / norm) : vec;
    }
}

function cosine(a: number[], b: number[]): number {
    let s = 0;
    for (let i = 0; i < a.length; i++) s += a[i] * b[i]; // both L2-normalized
    return s;
}

/** An embedding-backed knowledge store with cosine-similarity retrieval. */
export class VectorKnowledge implements Knowledge {
    private readonly docs: Array<{ emb: number[]; content: string; source: string }> = [];

    constructor(private readonly embedder: Embedder = new HashEmbedder()) {}

    ingest(content: string, source: string): void {
        this.docs.push({ emb: this.embedder.embed(content), content, source });
    }

    query(query: string, topK = 4): KnowledgeHit[] {
        if (topK <= 0 || this.docs.length === 0) return [];
        const q = this.embedder.embed(query);
        return this.docs
            .map((d) => ({ content: d.content, source: d.source, score: cosine(q, d.emb) }))
            .filter((h) => h.score > 0)
            .sort((a, b) => b.score - a.score)
            .slice(0, topK);
    }
}
