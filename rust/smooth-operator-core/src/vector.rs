//! Vector knowledge — embedding-backed semantic retrieval.
//!
//! Sibling of the four ports' vector store ([`crate::knowledge::InMemoryKnowledge`]
//! is the lexical one). [`VectorKnowledge`] embeds chunks and queries and retrieves
//! by cosine similarity, satisfying the same [`KnowledgeBase`] trait as the lexical
//! retriever — so the agent accepts either. The [`Embedder`] is pluggable: the
//! default [`HashEmbedder`] is deterministic and offline (feature-hashed
//! bag-of-words), good for tests and a zero-dependency default, while a gateway
//! embedder drops in behind the same trait for true semantics.

use std::sync::Mutex;

use crate::knowledge::{tokenize, Document, InMemoryKnowledge, KnowledgeBase, KnowledgeResult};

/// A small deterministic non-cryptographic hash (FNV-1a, 32-bit).
#[must_use]
pub fn hash_token(token: &str) -> u32 {
    let mut h: u32 = 0x811c_9dc5;
    for byte in token.as_bytes() {
        h ^= u32::from(*byte);
        h = h.wrapping_mul(0x0100_0193);
    }
    h
}

/// Turns text into a fixed-length vector.
pub trait Embedder: Send + Sync {
    /// Embed `text` as a fixed-length vector. Implementations must L2-normalize
    /// (or return the zero vector) — [`VectorKnowledge`] scores with a plain dot
    /// product.
    fn embed(&self, text: &str) -> Vec<f32>;
}

/// Deterministic, offline feature-hashing embedder. Hashes each token into one of
/// `dim` buckets (signed) and L2-normalizes. No learned semantics, but a real
/// vector with cosine geometry — docs sharing tokens land near each other.
#[derive(Debug, Clone, Copy)]
pub struct HashEmbedder {
    dim: usize,
}

impl HashEmbedder {
    /// Construct with the given dimension; `0` means the 256 default.
    #[must_use]
    pub fn new(dim: usize) -> Self {
        Self {
            dim: if dim == 0 { 256 } else { dim },
        }
    }
}

impl Default for HashEmbedder {
    fn default() -> Self {
        Self::new(256)
    }
}

impl Embedder for HashEmbedder {
    fn embed(&self, text: &str) -> Vec<f32> {
        let mut vec = vec![0.0f32; self.dim];
        for token in tokenize(text) {
            let h = hash_token(&token);
            let bucket = h as usize % self.dim;
            if (h >> 31) & 1 == 1 {
                vec[bucket] -= 1.0;
            } else {
                vec[bucket] += 1.0;
            }
        }
        let norm = vec.iter().map(|v| v * v).sum::<f32>().sqrt();
        if norm > 0.0 {
            for v in &mut vec {
                *v /= norm;
            }
        }
        vec
    }
}

/// Dot product of two L2-normalized vectors = cosine similarity.
fn cosine(a: &[f32], b: &[f32]) -> f32 {
    a.iter().zip(b).map(|(x, y)| x * y).sum()
}

struct VecChunk {
    embedding: Vec<f32>,
    document_id: String,
    source: String,
    chunk: String,
}

/// An embedding-backed knowledge store with cosine-similarity retrieval.
#[allow(missing_debug_implementations)]
pub struct VectorKnowledge {
    embedder: Box<dyn Embedder>,
    chunks: Mutex<Vec<VecChunk>>,
}

impl VectorKnowledge {
    /// Construct with the given embedder.
    #[must_use]
    pub fn new(embedder: Box<dyn Embedder>) -> Self {
        Self {
            embedder,
            chunks: Mutex::new(Vec::new()),
        }
    }
}

impl Default for VectorKnowledge {
    /// The offline [`HashEmbedder`] default — same as the ports'.
    fn default() -> Self {
        Self::new(Box::new(HashEmbedder::default()))
    }
}

impl KnowledgeBase for VectorKnowledge {
    fn ingest(&self, doc: Document) -> anyhow::Result<()> {
        let mut store = self.chunks.lock().map_err(|e| anyhow::anyhow!("lock poisoned: {e}"))?;
        for chunk in InMemoryKnowledge::chunk_content(&doc.content) {
            store.push(VecChunk {
                embedding: self.embedder.embed(&chunk),
                document_id: doc.id.clone(),
                source: doc.source.clone(),
                chunk,
            });
        }
        Ok(())
    }

    fn query(&self, query: &str, limit: usize) -> anyhow::Result<Vec<KnowledgeResult>> {
        if limit == 0 {
            return Ok(Vec::new());
        }
        let store = self.chunks.lock().map_err(|e| anyhow::anyhow!("lock poisoned: {e}"))?;
        if store.is_empty() {
            return Ok(Vec::new());
        }

        let q = self.embedder.embed(query);
        let mut hits: Vec<KnowledgeResult> = store
            .iter()
            .filter_map(|c| {
                let score = cosine(&q, &c.embedding);
                (score > 0.0).then(|| KnowledgeResult {
                    document_id: c.document_id.clone(),
                    chunk: c.chunk.clone(),
                    score,
                    source: c.source.clone(),
                })
            })
            .collect();

        // Stable descending sort — ties keep ingest order, as in the ports.
        hits.sort_by(|a, b| b.score.partial_cmp(&a.score).unwrap_or(std::cmp::Ordering::Equal));
        hits.truncate(limit);
        Ok(hits)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::knowledge::DocumentType;

    fn doc(content: &str, source: &str) -> Document {
        Document::new(content, source, DocumentType::Documentation)
    }

    #[test]
    fn hash_embedder_is_deterministic_and_normalized() {
        let embedder = HashEmbedder::new(64);
        let a = embedder.embed("return policy details");
        let b = embedder.embed("return policy details");
        assert_eq!(a.len(), 64);
        assert_eq!(a, b);
        let norm = a.iter().map(|v| v * v).sum::<f32>().sqrt();
        assert!((norm - 1.0).abs() < 1e-6, "not L2-normalized: {norm}");
    }

    #[test]
    fn hash_token_matches_the_ports_fnv1a() {
        // Same constants as the Go/TS/Python/.NET ports — a golden value keeps the
        // five engines' embeddings comparable.
        assert_eq!(hash_token(""), 0x811c_9dc5);
        assert_eq!(hash_token("a"), 0xe40c_292c);
    }

    #[test]
    fn empty_text_embeds_to_the_zero_vector() {
        let vec = HashEmbedder::new(8).embed("");
        assert_eq!(vec, vec![0.0; 8]);
    }

    #[test]
    fn retrieves_the_most_similar_document() {
        let kb = VectorKnowledge::new(Box::new(HashEmbedder::new(256)));
        kb.ingest(doc("Our return policy allows refunds within 30 days.", "returns.md"))
            .expect("ingest");
        kb.ingest(doc("The office is open Monday through Friday.", "hours.md")).expect("ingest");

        let hits = kb.query("how do refunds and returns work?", 1).expect("query");
        assert_eq!(hits.len(), 1);
        assert_eq!(hits[0].source, "returns.md");
        assert!(hits[0].score > 0.0);
    }

    #[test]
    fn empty_store_returns_nothing() {
        assert!(VectorKnowledge::default().query("anything", 4).expect("query").is_empty());
    }

    #[test]
    fn zero_limit_returns_nothing() {
        let kb = VectorKnowledge::default();
        kb.ingest(doc("anything at all", "a.md")).expect("ingest");
        assert!(kb.query("anything", 0).expect("query").is_empty());
    }

    #[test]
    fn unrelated_query_scores_nothing() {
        let kb = VectorKnowledge::default();
        kb.ingest(doc("gift wrapping costs 4.99 per item", "wrapping.md")).expect("ingest");
        assert!(kb.query("kubernetes cluster autoscaling", 4).expect("query").is_empty());
    }

    #[test]
    fn ingest_chunks_on_double_newlines() {
        let kb = VectorKnowledge::default();
        kb.ingest(doc("rust programming language\n\npython data science", "doc.md")).expect("ingest");
        let hits = kb.query("python", 4).expect("query");
        assert_eq!(hits.len(), 1, "only the python chunk should match: {hits:?}");
        assert_eq!(hits[0].chunk, "python data science");
    }
}
