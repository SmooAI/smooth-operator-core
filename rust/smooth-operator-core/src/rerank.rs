//! Knowledge reranking — reorder retrieved documents by relevance.
//!
//! A [`Reranker`] takes the query and the retriever's candidate hits and returns
//! them reordered (and possibly trimmed). [`NoopReranker`] is the zero-cost
//! passthrough; [`LexicalReranker`] re-scores by query-term coverage normalized for
//! document length, so a concise on-topic doc outranks a long one with the same raw
//! overlap. A cross-encoder or gateway reranker drops in behind the same trait.
//!
//! The agent applies it between retrieval and context injection — see
//! [`crate::AgentConfig::with_reranker`].

use crate::knowledge::{tokenize, KnowledgeResult};
use std::collections::HashSet;

/// Reorders retrieved hits by relevance to the query.
pub trait Reranker: Send + Sync {
    /// Return `hits` reordered (and possibly trimmed) by relevance to `query`.
    fn rerank(&self, query: &str, hits: Vec<KnowledgeResult>) -> Vec<KnowledgeResult>;
}

/// Returns the hits unchanged — the zero-cost default.
#[derive(Debug, Clone, Copy, Default)]
pub struct NoopReranker;

impl Reranker for NoopReranker {
    fn rerank(&self, _query: &str, hits: Vec<KnowledgeResult>) -> Vec<KnowledgeResult> {
        hits
    }
}

/// Reorders by query-term coverage normalized by document length:
/// `coverage / log2(2 + doc_token_count)`, so coverage is rewarded but long
/// documents are penalized relative to concise ones with the same coverage.
/// Stable for ties.
#[derive(Debug, Clone, Copy, Default)]
pub struct LexicalReranker;

impl Reranker for LexicalReranker {
    fn rerank(&self, query: &str, mut hits: Vec<KnowledgeResult>) -> Vec<KnowledgeResult> {
        let query_terms: HashSet<String> = tokenize(query).into_iter().collect();
        if query_terms.is_empty() {
            return hits;
        }

        let score = |hit: &KnowledgeResult| -> f64 {
            let doc_tokens = tokenize(&hit.chunk);
            let coverage = doc_tokens.iter().collect::<HashSet<_>>().iter().filter(|t| query_terms.contains(**t)).count();
            #[allow(clippy::cast_precision_loss)]
            {
                coverage as f64 / (2.0 + doc_tokens.len() as f64).log2()
            }
        };

        // `sort_by` is stable, so equal scores keep the retriever's order.
        hits.sort_by(|a, b| score(b).partial_cmp(&score(a)).unwrap_or(std::cmp::Ordering::Equal));
        hits
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hit(chunk: &str) -> KnowledgeResult {
        KnowledgeResult {
            document_id: "d".to_string(),
            chunk: chunk.to_string(),
            score: 0.0,
            source: "s".to_string(),
        }
    }

    #[test]
    fn noop_is_passthrough() {
        let out = NoopReranker.rerank("q", vec![hit("a"), hit("b")]);
        assert_eq!(out.len(), 2);
        assert_eq!(out[0].chunk, "a");
    }

    #[test]
    fn lexical_prefers_the_concise_document() {
        let verbose = format!("return {}policy", "filler ".repeat(60));
        let out = LexicalReranker.rerank("return policy", vec![hit(&verbose), hit("return policy")]);
        assert_eq!(out[0].chunk, "return policy");
    }

    #[test]
    fn lexical_prefers_higher_coverage() {
        let out = LexicalReranker.rerank("return policy", vec![hit("return shipping details"), hit("return and policy details")]);
        assert_eq!(out[0].chunk, "return and policy details");
    }

    #[test]
    fn empty_query_is_passthrough() {
        let out = LexicalReranker.rerank("", vec![hit("a"), hit("b")]);
        assert_eq!(out[0].chunk, "a");
    }

    #[test]
    fn ties_keep_the_retrievers_order() {
        let out = LexicalReranker.rerank("return policy", vec![hit("return policy one"), hit("return policy two")]);
        assert_eq!(out[0].chunk, "return policy one");
        assert_eq!(out[1].chunk, "return policy two");
    }
}
