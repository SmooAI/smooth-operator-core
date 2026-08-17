---
"@smooai/smooth-operator-core": patch
---

feat(rust): vector store + lexical reranker, so the Rust reference stops being the one engine without them

The inverted parity gap: Go, TypeScript, Python and .NET all shipped a vector
store and a reranker, while the Rust reference — the engine every SmooAI
production brain actually runs — had neither. `docs/Polyglot-Engines.md`
nonetheless listed "Rerank — lexical reranker built in" as a shared feature, with
an asterisk saying Rust delegates it to its host. Rust now has both, and the
asterisk is gone.

- **`vector::VectorKnowledge`** — an embedding-backed `KnowledgeBase` with cosine
  retrieval, alongside the existing lexical `InMemoryKnowledge`. The `Embedder`
  seam is pluggable; the default `HashEmbedder` is the ports' deterministic
  offline feature-hashing embedder (FNV-1a token hash → signed buckets →
  L2-normalize), so it needs no network and no credentials. It reuses the crate's
  existing chunker on ingest, so a `VectorKnowledge` is a drop-in swap for
  `InMemoryKnowledge`.
- **`rerank::Reranker`** — `NoopReranker` (passthrough) and `LexicalReranker`,
  scoring `coverage / log2(2 + doc_token_count)` exactly as the ports do, so a
  concise on-topic doc outranks a long one with the same raw overlap. Stable for
  ties, so equal scores keep the retriever's order.
- **Wired into the agent**, not left as library furniture:
  `AgentConfig::with_reranker(reranker, candidate_k)` pulls a candidate pool from
  the retriever, reranks it, and truncates to `KNOWLEDGE_TOP_K` before injecting
  the `[Relevant knowledge]` block. Unset = the previous behavior byte for byte.

Parity was checked against Go on a shared corpus, not asserted: the same four
documents and three queries produce identical cosine scores to six decimals in
both engines (`0.129099`, `0.109109`, `0.176777`, …) and the reranker returns the
identical order, ties included.
