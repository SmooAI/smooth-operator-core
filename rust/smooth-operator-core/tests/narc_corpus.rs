//! Narc detection corpus — the Rust half of the cross-language drift gate.
//!
//! `spec/narc/corpus.json` was generated FROM this engine, which is the normative
//! implementation of the secret + prompt-injection detectors: the Go, TypeScript,
//! Python and .NET ports all replay the same file and must produce identical
//! findings, in the same order, at the same severities.
//!
//! That only holds if the reference is pinned too. Without this test a change to
//! `narc.rs` would move Rust and leave the other four engines silently enforcing
//! the old contract — the exact failure mode the corpus exists to prevent. So this
//! asserts Rust still agrees with the file: change a pattern, and this fails until
//! the corpus (and therefore every port) is updated with it.

use smooth_operator_core::narc::{has_injection, has_secrets, scan_injection, scan_secrets, Finding};

/// A ratchet: the shared corpus may grow, never shrink. A deleted vector is a
/// silently weakened detector in five languages at once.
const MIN_VECTORS: usize = 39;

fn render(findings: &[Finding]) -> Vec<String> {
    findings.iter().map(|f| format!("{}|{}", f.pattern_name, f.severity)).collect()
}

#[test]
fn narc_matches_the_shared_corpus() {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../spec/narc/corpus.json");
    let raw = std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {}: {e}", path.display()));
    let corpus: serde_json::Value = serde_json::from_str(&raw).expect("corpus parses");
    let vectors = corpus["vectors"].as_array().expect("corpus has a vectors array");

    assert!(
        vectors.len() >= MIN_VECTORS,
        "corpus shrank: {} vectors < ratchet floor {MIN_VECTORS} — a vector was deleted from spec/narc/corpus.json",
        vectors.len()
    );

    for v in vectors {
        let id = v["id"].as_str().expect("vector id");
        let text = v["text"].as_str().expect("vector text");
        let want_secrets: Vec<String> = v["secrets"]
            .as_array()
            .expect("secrets")
            .iter()
            .map(|s| s.as_str().unwrap().to_string())
            .collect();
        let want_injection: Vec<String> = v["injection"]
            .as_array()
            .expect("injection")
            .iter()
            .map(|s| s.as_str().unwrap().to_string())
            .collect();

        assert_eq!(render(&scan_secrets(text)), want_secrets, "secrets mismatch for vector {id}");
        assert_eq!(render(&scan_injection(text)), want_injection, "injection mismatch for vector {id}");
        // The convenience predicates must agree with the scanners.
        assert_eq!(has_secrets(text), !want_secrets.is_empty(), "has_secrets disagrees for vector {id}");
        assert_eq!(has_injection(text), !want_injection.is_empty(), "has_injection disagrees for vector {id}");
    }
}
