//! LLM-as-judge eval suite for the Rust reference engine.
//!
//! Rust is the reference implementation, and until now it ran ZERO eval scenarios
//! — the other four languages each carried a hand-copied corpus that cited a
//! `rust/evals` directory which has never existed in this repo. This is that
//! suite, reading the SHARED corpus at `spec/evals/scenarios.json`; nothing is
//! defined here. See that file's `$comment`.
//!
//! Two tests live here:
//!
//! - `eval_corpus_matches_spec` — OFFLINE, always runs. The drift guard.
//! - `eval_aggregate_mean_clears_threshold` — gated on `SMOOTH_AGENT_E2E=1` +
//!   `SMOOAI_GATEWAY_KEY`, so it's a no-op (never fails) without credentials:
//!
//!   ```text
//!   SMOOAI_GATEWAY_KEY=... SMOOTH_AGENT_E2E=1 cargo test -p smooai-smooth-operator-core --test evals
//!   ```

use std::collections::HashMap;
use std::sync::Arc;

use serde::Deserialize;
use smooth_operator_core::llm::{ApiFormat, RetryPolicy};
use smooth_operator_core::{Agent, AgentConfig, Document, DocumentType, InMemoryKnowledge, KnowledgeBase, LlmClient, LlmConfig, Message, ToolRegistry};

const GATEWAY_URL: &str = "https://llm.smoo.ai/v1";
const DEFAULT_MODEL: &str = "claude-haiku-4-5";

/// A RATCHET, not a duplicate of the corpus. Comparing the loaded set against the
/// file catches a language that subsets or mis-parses it, but not a scenario
/// deleted from the file itself — both sides shrink together and every language
/// stays green. This floor is what makes a deletion loud. Raise it when you add
/// scenarios; lowering it should require saying why in the PR.
const MIN_SCENARIOS: usize = 15;

/// The shared corpus, embedded at compile time from the single source of truth.
/// `include_str!` means a moved or deleted corpus is a BUILD failure, not a
/// runtime surprise in a nightly.
const CORPUS_JSON: &str = include_str!("../../../spec/evals/scenarios.json");

#[derive(Debug, Deserialize)]
struct EvalDoc {
    content: String,
    source: String,
}

#[derive(Debug, Deserialize)]
struct EvalScenario {
    id: String,
    tier: String,
    #[allow(dead_code)]
    intent: String,
    kb_docs: Vec<String>,
    user_turns: Vec<String>,
    ground_truth: String,
    rubric: String,
}

#[derive(Debug, Deserialize)]
struct EvalCorpus {
    support_prompt: String,
    judge_system_prompt: String,
    aggregate_mean_threshold: f64,
    hard_aggregate_mean_threshold: f64,
    docs: HashMap<String, EvalDoc>,
    scenarios: Vec<EvalScenario>,
}

fn corpus() -> EvalCorpus {
    serde_json::from_str(CORPUS_JSON).expect("spec/evals/scenarios.json parses")
}

/// Resolve a scenario's `kb_docs` keys into the documents to seed.
fn docs_for<'a>(corpus: &'a EvalCorpus, scenario: &EvalScenario) -> Vec<&'a EvalDoc> {
    scenario
        .kb_docs
        .iter()
        .map(|key| {
            corpus
                .docs
                .get(key)
                .unwrap_or_else(|| panic!("scenario {} references unknown doc {key:?}", scenario.id))
        })
        .collect()
}

/// The drift guard: runs OFFLINE in normal CI. Asserts the scenario set this
/// suite would execute is exactly the set in `spec/evals/scenarios.json` — same
/// count, same ids — so a language that subsets, filters or mis-parses the corpus
/// goes red here instead of quietly running a forked suite (which is how the .NET
/// corpus drifted).
#[test]
fn eval_corpus_matches_spec() {
    let corpus = corpus();

    // The raw ids in the file, independent of the typed decode above.
    let raw: serde_json::Value = serde_json::from_str(CORPUS_JSON).expect("corpus parses as json");
    let mut file_ids: Vec<String> = raw["scenarios"]
        .as_array()
        .expect("scenarios is an array")
        .iter()
        .map(|s| s["id"].as_str().expect("scenario id is a string").to_string())
        .collect();
    let mut loaded_ids: Vec<String> = corpus.scenarios.iter().map(|s| s.id.clone()).collect();

    assert_eq!(
        loaded_ids.len(),
        file_ids.len(),
        "corpus count drift: loaded {}, spec has {}",
        loaded_ids.len(),
        file_ids.len()
    );
    file_ids.sort();
    loaded_ids.sort();
    assert_eq!(loaded_ids, file_ids, "corpus id drift");

    let unique: std::collections::HashSet<&String> = loaded_ids.iter().collect();
    assert_eq!(unique.len(), loaded_ids.len(), "duplicate scenario ids");

    assert!(
        corpus.scenarios.len() >= MIN_SCENARIOS,
        "corpus shrank: {} scenarios < ratchet floor {MIN_SCENARIOS} — a scenario was deleted from spec/evals/scenarios.json",
        corpus.scenarios.len()
    );

    // Every scenario must be runnable: resolvable docs, and a non-empty prompt,
    // ground truth and rubric. Catches a malformed corpus before a nightly burns
    // gateway spend discovering it.
    for scenario in &corpus.scenarios {
        assert!(!scenario.user_turns.is_empty(), "{} has no user turns", scenario.id);
        assert!(!scenario.ground_truth.is_empty(), "{} has no ground truth", scenario.id);
        assert!(!scenario.rubric.is_empty(), "{} has no rubric", scenario.id);
        docs_for(&corpus, scenario);
    }

    assert!(corpus.scenarios.iter().any(|s| s.tier == "core"), "corpus has no core-tier scenarios");
    assert!(!corpus.support_prompt.is_empty() && !corpus.judge_system_prompt.is_empty());

    let core = corpus.scenarios.iter().filter(|s| s.tier == "core").count();
    println!("[rust-eval] corpus in sync: {} scenarios ({core} core)", corpus.scenarios.len());
}

/// Extract the judge's JSON verdict, tolerating markdown fences / stray prose.
fn parse_verdict(text: &str) -> anyhow::Result<(i64, String)> {
    let start = text.find('{').ok_or_else(|| anyhow::anyhow!("judge did not return JSON: {text}"))?;
    let end = text.rfind('}').ok_or_else(|| anyhow::anyhow!("judge did not return JSON: {text}"))?;
    let v: serde_json::Value = serde_json::from_str(&text[start..=end])?;
    let score = v["score"].as_i64().ok_or_else(|| anyhow::anyhow!("verdict has no numeric score"))?;
    let reasoning = v["reasoning"].as_str().unwrap_or_default().to_string();
    Ok((score, reasoning))
}

fn gateway_config(api_key: &str, model: &str) -> LlmConfig {
    LlmConfig {
        api_url: GATEWAY_URL.to_string(),
        api_key: api_key.to_string(),
        model: model.to_string(),
        max_tokens: 1024,
        temperature: 0.0,
        retry_policy: RetryPolicy::default(),
        api_format: ApiFormat::OpenAiCompat,
    }
}

#[tokio::test]
async fn eval_aggregate_mean_clears_threshold() {
    if std::env::var("SMOOTH_AGENT_E2E").as_deref() != Ok("1") {
        eprintln!("SMOOTH_AGENT_E2E != \"1\" — skipping live-gateway eval suite.");
        return;
    }
    let Ok(api_key) = std::env::var("SMOOAI_GATEWAY_KEY") else {
        eprintln!("SMOOAI_GATEWAY_KEY unset — skipping live-gateway eval suite.");
        return;
    };
    if api_key.is_empty() {
        eprintln!("SMOOAI_GATEWAY_KEY empty — skipping live-gateway eval suite.");
        return;
    }

    let corpus = corpus();
    // `.filter(!empty)` because the nightly workflow exports the var from a
    // workflow_dispatch input — EMPTY STRING on cron/blank dispatch, not unset.
    // An empty model reaches the gateway as `model=` → 400 (first live run).
    let judge_model = std::env::var("SMOOTH_AGENT_JUDGE_MODEL")
        .ok()
        .filter(|m| !m.trim().is_empty())
        .unwrap_or_else(|| DEFAULT_MODEL.to_string());
    let judge = LlmClient::new(gateway_config(&api_key, &judge_model));

    // Tiers are scored separately: core must clear the real bar, hard sits on a
    // lenient floor so one adversarial miss is an improvement target, not a red CI.
    let mut totals: HashMap<&str, (i64, usize)> = HashMap::new();

    for scenario in &corpus.scenarios {
        let knowledge = InMemoryKnowledge::new();
        for doc in docs_for(&corpus, scenario) {
            knowledge
                .ingest(Document::new(&doc.content, &doc.source, DocumentType::Documentation))
                .expect("ingest");
        }

        // Earlier turns are replayed as prior_messages so the last turn — the one
        // the judge scores — sees the full multi-turn context.
        let (last_turn, earlier) = scenario.user_turns.split_last().expect("at least one turn");
        let mut config = AgentConfig::new("eval-agent", &corpus.support_prompt, gateway_config(&api_key, DEFAULT_MODEL));
        config.knowledge = Some(Arc::new(knowledge));
        config.prior_messages = earlier.iter().map(Message::user).collect();

        let agent = Agent::new(config, ToolRegistry::new());
        let conversation = agent.run(last_turn.clone()).await.expect("agent run");
        let reply = conversation.last_assistant_content().unwrap_or_default().to_string();

        let judge_user = format!(
            "GROUND TRUTH:\n{}\n\nRUBRIC:\n{}\n\nAGENT REPLY:\n{}\n\nScore it now as JSON.",
            scenario.ground_truth, scenario.rubric, reply
        );
        let sys = Message::system(&corpus.judge_system_prompt);
        let usr = Message::user(judge_user);
        let verdict = judge.chat(&[&sys, &usr], &[]).await.expect("judge call");
        let (score, reasoning) = parse_verdict(&verdict.content).expect("parse verdict");

        let entry = totals.entry(scenario.tier.as_str()).or_insert((0, 0));
        entry.0 += score;
        entry.1 += 1;
        println!("[rust-eval] ({}) {}: {score}/5 — {reasoning}", scenario.tier, scenario.id);
    }

    let mut failures = Vec::new();
    for (tier, threshold) in [("core", corpus.aggregate_mean_threshold), ("hard", corpus.hard_aggregate_mean_threshold)] {
        let Some(&(total, count)) = totals.get(tier) else { continue };
        if count == 0 {
            continue;
        }
        #[allow(clippy::cast_precision_loss)]
        let mean = total as f64 / count as f64;
        println!("[rust-eval] {tier} aggregate mean {mean:.2}/5 across {count} scenarios");
        if mean < threshold {
            failures.push(format!("{tier} aggregate mean {mean:.2} < {threshold}"));
        }
    }
    assert!(failures.is_empty(), "{}", failures.join("; "));
}
