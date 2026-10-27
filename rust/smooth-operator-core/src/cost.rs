//! Cost tracking and budget enforcement for LLM usage.
//!
//! Provides [`CostTracker`] for accumulating token usage and costs across
//! multiple LLM calls, [`CostBudget`] for setting spending limits, and
//! [`ModelPricing`] with built-in presets for common models.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::collections::hash_map::DefaultHasher;
use std::collections::{BTreeMap, HashMap};
use std::fmt;
use std::hash::{Hash, Hasher};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use crate::llm::Usage;

/// Tracks cumulative LLM cost and token usage across an agent session.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct CostTracker {
    pub total_prompt_tokens: u64,
    pub total_completion_tokens: u64,
    /// Subset of `total_prompt_tokens` that hit Anthropic's prompt cache.
    /// Aggregated from `Usage::cached_tokens` (which comes from the OpenAI-shaped
    /// `usage.prompt_tokens_details.cached_tokens` field). Pearl
    /// th-litellm-caching-client.
    #[serde(default)]
    pub total_cached_tokens: u64,
    pub total_cost_usd: f64,
    pub calls: u32,
    /// True once any call in this run was priced from the local
    /// [`ModelPricing`] table instead of the gateway's own figure, which taints
    /// `total_cost_usd` as an ESTIMATE. Local pricing cannot price aliased
    /// routes and returns the free tier for any model it doesn't recognise, so
    /// nothing may present a tainted total as a measured cost — a billed SKU
    /// especially. Pearl th-126fe6.
    #[serde(default)]
    pub cost_estimated: bool,
    /// True once any call in this run reported no usage and had its token
    /// counts estimated from character counts (`LlmResponse::usage_estimated`).
    /// Independent of [`Self::cost_estimated`]: the gateway sends cost on an
    /// HTTP header and usage on an SSE chunk, so either can be authoritative
    /// while the other is a guess.
    #[serde(default)]
    pub usage_estimated: bool,
    /// The most recent call's gateway response id (`chatcmpl-…`), the join key
    /// to `LiteLLM_SpendLogs.request_id`.
    ///
    /// ponytail: LAST call only, because the span it feeds has exactly one
    /// `response_id` column. A multi-call turn therefore joins to its final
    /// call, not the whole turn. Per-LLM-call inference spans are the upgrade
    /// path if a turn's full spend ever needs reconstructing.
    #[serde(default)]
    pub last_response_id: Option<String>,
    entries: Vec<CostEntry>,
}

/// A single recorded LLM call with its cost breakdown.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CostEntry {
    pub model: String,
    pub prompt_tokens: u32,
    pub completion_tokens: u32,
    pub cost_usd: f64,
    pub timestamp: DateTime<Utc>,
}

/// Budget limits for an agent session.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CostBudget {
    pub max_cost_usd: Option<f64>,
    pub max_tokens: Option<u64>,
}

/// Per-model pricing in USD per million tokens.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ModelPricing {
    /// USD per million input/prompt tokens.
    pub prompt_per_mtok: f64,
    /// USD per million output/completion tokens.
    pub completion_per_mtok: f64,
}

/// Error returned when a budget limit has been exceeded.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct BudgetExceeded {
    pub spent_usd: f64,
    pub limit_usd: Option<f64>,
    pub total_tokens: u64,
    pub limit_tokens: Option<u64>,
}

impl fmt::Display for BudgetExceeded {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "budget exceeded: spent ${:.4}", self.spent_usd)?;
        if let Some(limit) = self.limit_usd {
            write!(f, " (limit ${limit:.4})")?;
        }
        write!(f, ", {} tokens", self.total_tokens)?;
        if let Some(limit) = self.limit_tokens {
            write!(f, " (limit {limit})")?;
        }
        Ok(())
    }
}

impl std::error::Error for BudgetExceeded {}

impl CostTracker {
    /// Record a single LLM call's usage and cost.
    pub fn record(&mut self, model: &str, usage: &Usage, pricing: &ModelPricing) {
        let cost = pricing.calculate(usage.prompt_tokens, usage.completion_tokens);
        self.record_with_cost(model, usage, cost);
        // Taint AFTER the delegate, which resets nothing — this is the local
        // pricing path, so the running total is no longer gateway-authoritative.
        self.cost_estimated = true;
    }

    /// Record a single LLM call with an explicit cost (e.g. the
    /// gateway's authoritative number from the `x-litellm-response-cost`
    /// response header). Prefer this over [`record`] whenever the
    /// gateway reports a cost — local `ModelPricing` can't price
    /// aliased routes (`smooth-coding` → unknown upstream) accurately.
    pub fn record_with_cost(&mut self, model: &str, usage: &Usage, cost_usd: f64) {
        self.total_prompt_tokens += u64::from(usage.prompt_tokens);
        self.total_completion_tokens += u64::from(usage.completion_tokens);
        self.total_cached_tokens += u64::from(usage.cached_tokens);
        self.total_cost_usd += cost_usd;
        self.calls += 1;

        self.entries.push(CostEntry {
            model: model.to_string(),
            prompt_tokens: usage.prompt_tokens,
            completion_tokens: usage.completion_tokens,
            cost_usd,
            timestamp: Utc::now(),
        });
    }

    /// Check whether the current totals exceed the given budget.
    ///
    /// # Errors
    /// Returns [`BudgetExceeded`] if either the USD or token limit is breached.
    pub fn check_budget(&self, budget: &CostBudget) -> Result<(), BudgetExceeded> {
        let total_tokens = self.total_prompt_tokens + self.total_completion_tokens;

        let usd_exceeded = budget.max_cost_usd.is_some_and(|limit| self.total_cost_usd > limit);
        let tokens_exceeded = budget.max_tokens.is_some_and(|limit| total_tokens > limit);

        if usd_exceeded || tokens_exceeded {
            return Err(BudgetExceeded {
                spent_usd: self.total_cost_usd,
                limit_usd: budget.max_cost_usd,
                total_tokens,
                limit_tokens: budget.max_tokens,
            });
        }

        Ok(())
    }

    /// Return all recorded cost entries.
    pub fn entries(&self) -> &[CostEntry] {
        &self.entries
    }

    /// Reset the tracker to its initial state.
    pub fn reset(&mut self) {
        self.total_prompt_tokens = 0;
        self.total_completion_tokens = 0;
        self.total_cached_tokens = 0;
        self.total_cost_usd = 0.0;
        self.cost_estimated = false;
        self.usage_estimated = false;
        self.last_response_id = None;
        self.calls = 0;
        self.entries.clear();
    }
}

impl ModelPricing {
    /// Calculate cost in USD for a given number of prompt and completion tokens.
    fn calculate(&self, prompt_tokens: u32, completion_tokens: u32) -> f64 {
        let prompt_cost = f64::from(prompt_tokens) * self.prompt_per_mtok / 1_000_000.0;
        let completion_cost = f64::from(completion_tokens) * self.completion_per_mtok / 1_000_000.0;
        prompt_cost + completion_cost
    }

    /// GPT-4o pricing.
    #[must_use]
    pub fn gpt_4o() -> Self {
        Self {
            prompt_per_mtok: 2.50,
            completion_per_mtok: 10.00,
        }
    }

    /// GPT-4o Mini pricing.
    #[must_use]
    pub fn gpt_4o_mini() -> Self {
        Self {
            prompt_per_mtok: 0.15,
            completion_per_mtok: 0.60,
        }
    }

    /// DeepSeek V3 pricing.
    #[must_use]
    pub fn deepseek_v3() -> Self {
        Self {
            prompt_per_mtok: 0.27,
            completion_per_mtok: 1.10,
        }
    }

    /// DeepSeek R1 pricing.
    #[must_use]
    pub fn deepseek_r1() -> Self {
        Self {
            prompt_per_mtok: 0.55,
            completion_per_mtok: 2.19,
        }
    }

    /// Gemini Flash pricing.
    #[must_use]
    pub fn gemini_flash() -> Self {
        Self {
            prompt_per_mtok: 0.075,
            completion_per_mtok: 0.30,
        }
    }

    /// Look up pricing for a model name, falling back to free tier for unknown models.
    ///
    /// Includes Smoo's own routing aliases (`smooth-fast-gemini`,
    /// `smooth-coding`, …) so the local fallback returns a usable
    /// estimate when the gateway omits `x-litellm-response-cost`.
    /// Pearl th-431ba2: without these, the cost tracker stays at
    /// $0 for the entire dispatch because every smooth-* alias
    /// landed in the free tier.
    #[must_use]
    pub fn for_model(model: &str) -> Self {
        let m = model.to_lowercase();
        if m.contains("gpt-4o-mini") {
            Self::gpt_4o_mini()
        } else if m.contains("gpt-4o") {
            Self::gpt_4o()
        } else if m.contains("deepseek") && m.contains("r1") {
            Self::deepseek_r1()
        } else if m.contains("deepseek") {
            Self::deepseek_v3()
        } else if m.contains("gemini") && m.contains("flash") {
            Self::gemini_flash()
        } else if m.contains("smooth-fast-gemini") {
            // routes to gemini-flash on the gateway
            Self::gemini_flash()
        } else if m.contains("smooth-coding") {
            // routes through MiniMax-style coding upstream; use
            // gpt_4o_mini as a rough peer until we have a real
            // local entry. Better an estimate than $0.
            Self::gpt_4o_mini()
        } else {
            Self::free()
        }
    }

    /// Free tier / local model pricing.
    #[must_use]
    pub fn free() -> Self {
        Self {
            prompt_per_mtok: 0.0,
            completion_per_mtok: 0.0,
        }
    }
}

/// How long a fetched gateway price book is trusted before it is refetched.
const PRICE_BOOK_TTL: Duration = Duration::from_secs(60 * 60);
/// How long a FAILED fetch is remembered, so a gateway without `/model/info`
/// (OpenAI, OpenRouter) costs one request per window rather than one per call.
const PRICE_BOOK_FAILURE_TTL: Duration = Duration::from_secs(5 * 60);
/// A turn waits on this fetch at most once per TTL, so keep it short.
const PRICE_BOOK_FETCH_TIMEOUT: Duration = Duration::from_secs(5);

struct CachedPriceBook {
    fetched_at: Instant,
    /// `None` = the fetch failed; remembered for [`PRICE_BOOK_FAILURE_TTL`].
    book: Option<Arc<BTreeMap<String, ModelPricing>>>,
}

fn price_book_cache() -> &'static Mutex<HashMap<u64, CachedPriceBook>> {
    static CACHE: OnceLock<Mutex<HashMap<u64, CachedPriceBook>>> = OnceLock::new();
    CACHE.get_or_init(|| Mutex::new(HashMap::new()))
}

/// Cache key for one gateway + credential. The key is hashed so the cache
/// never holds a second copy of the secret; different keys can see different
/// model lists, so they cache separately.
fn price_book_key(api_url: &str, api_key: &str) -> u64 {
    let mut h = DefaultHasher::new();
    api_url.hash(&mut h);
    api_key.hash(&mut h);
    h.finish()
}

/// The gateway's own published list price for `model`, from LiteLLM's
/// `/model/info` (`input_cost_per_token` / `output_cost_per_token`).
///
/// This is the fallback when a call carried no gateway cost, which is every
/// streamed call on llm.smoo.ai: its cost headers are flushed before the
/// completion is priced and read 0. It beats [`ModelPricing::for_model`],
/// which knows a handful of models and prices everything else (`gpt-6-luna`,
/// every `smooth-*` alias) at the free tier, so every turn recorded $0.
/// Multiplying REAL tokens by the gateway's own price is what LiteLLM itself
/// bills, short of cache hits. Pearl th-c420bf.
///
/// The price book is fetched once per gateway + key and cached process-wide
/// for an hour (a failure for five minutes). Returns `None` when the gateway
/// has no `/model/info`, the fetch fails, or the model is unpriced there.
pub async fn gateway_list_price(api_url: &str, api_key: &str, model: &str) -> Option<ModelPricing> {
    let key = price_book_key(api_url, api_key);
    let cached = {
        let cache = price_book_cache().lock().ok()?;
        cache.get(&key).and_then(|c| {
            let ttl = if c.book.is_some() { PRICE_BOOK_TTL } else { PRICE_BOOK_FAILURE_TTL };
            (c.fetched_at.elapsed() < ttl).then(|| c.book.clone())
        })
    };
    let book = match cached {
        Some(book) => book,
        None => {
            let book = match crate::resolution::fetch_model_pricing(api_url, api_key, PRICE_BOOK_FETCH_TIMEOUT).await {
                Ok(book) => Some(Arc::new(book)),
                Err(e) => {
                    tracing::debug!(error = %e, "gateway price book unavailable; falling back to local ModelPricing");
                    None
                }
            };
            if let Ok(mut cache) = price_book_cache().lock() {
                cache.insert(
                    key,
                    CachedPriceBook {
                        fetched_at: Instant::now(),
                        book: book.clone(),
                    },
                );
            }
            book
        }
    };
    book?.get(model).cloned()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::llm::Usage;
    use std::sync::atomic::{AtomicUsize, Ordering};

    /// Serve `status` + `body` for every request on a fresh port, counting
    /// hits. Returns the OpenAI-compat base URL (`…/v1`) and the counter.
    async fn model_info_server(status: &'static str, body: &'static str) -> (String, Arc<AtomicUsize>) {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let hits = Arc::new(AtomicUsize::new(0));
        let counter = Arc::clone(&hits);
        tokio::spawn(async move {
            loop {
                let Ok((mut sock, _)) = listener.accept().await else { return };
                counter.fetch_add(1, Ordering::SeqCst);
                let mut buf = [0u8; 4096];
                let _ = sock.read(&mut buf).await;
                let resp = format!(
                    "HTTP/1.1 {status}\r\ncontent-type: application/json\r\nconnection: close\r\ncontent-length: {}\r\n\r\n{body}",
                    body.len()
                );
                let _ = sock.write_all(resp.as_bytes()).await;
                let _ = sock.flush().await;
            }
        });
        (format!("http://{addr}/v1"), hits)
    }

    const MODEL_INFO: &str = r#"{"data":[
        {"model_name":"gpt-6-luna","model_info":{"input_cost_per_token":0.000000115,"output_cost_per_token":0.000000575}},
        {"model_name":"unpriced","model_info":{"input_cost_per_token":0.0,"output_cost_per_token":0.0}}
    ]}"#;

    /// The fallback that fixes `cost_usd = 0`: a model the local table prices
    /// at the free tier gets the gateway's own list price. Pearl th-c420bf.
    #[tokio::test]
    async fn gateway_list_price_reads_the_gateway_price_book() {
        let (url, _) = model_info_server("200 OK", MODEL_INFO).await;
        assert_eq!(ModelPricing::for_model("gpt-6-luna").prompt_per_mtok, 0.0, "the local table cannot price it");

        let p = gateway_list_price(&url, "k", "gpt-6-luna").await.expect("priced by the gateway");
        assert!((p.prompt_per_mtok - 0.115).abs() < 1e-9);
        assert!((p.completion_per_mtok - 0.575).abs() < 1e-9);
        // 1,000 in + 1,000 out at $0.115/M + $0.575/M.
        assert!((p.calculate(1000, 1000) - 0.000_69).abs() < 1e-12);
    }

    #[tokio::test]
    async fn gateway_list_price_is_none_for_unpriced_or_unknown_models() {
        let (url, _) = model_info_server("200 OK", MODEL_INFO).await;
        assert!(gateway_list_price(&url, "k", "unpriced").await.is_none(), "0/0 is unpriced, not free");
        assert!(gateway_list_price(&url, "k", "nope").await.is_none());
    }

    /// One fetch per gateway + key per TTL, not one per LLM call.
    #[tokio::test]
    async fn gateway_list_price_caches_the_price_book() {
        let (url, hits) = model_info_server("200 OK", MODEL_INFO).await;
        for _ in 0..3 {
            assert!(gateway_list_price(&url, "k", "gpt-6-luna").await.is_some());
        }
        assert_eq!(hits.load(Ordering::SeqCst), 1);
        // A different key may see a different model list, so it fetches its own.
        assert!(gateway_list_price(&url, "other-key", "gpt-6-luna").await.is_some());
        assert_eq!(hits.load(Ordering::SeqCst), 2);
    }

    /// A gateway without `/model/info` (OpenAI, OpenRouter) yields `None`, and
    /// the failure is remembered so it isn't retried on every call.
    #[tokio::test]
    async fn gateway_list_price_remembers_a_failed_fetch() {
        let (url, hits) = model_info_server("404 Not Found", r#"{"error":"not found"}"#).await;
        assert!(gateway_list_price(&url, "k", "gpt-6-luna").await.is_none());
        assert!(gateway_list_price(&url, "k", "gpt-6-luna").await.is_none());
        assert_eq!(hits.load(Ordering::SeqCst), 1);
    }

    /// Cost provenance: only a gateway-reported figure leaves the total
    /// authoritative. The local `ModelPricing` path taints it, because that
    /// table cannot price aliased routes and returns the FREE tier for any
    /// model it doesn't recognise — so a total built from it may be a wild
    /// under-count while looking exact. A billed surface has to be able to tell
    /// the two apart. Pearl th-126fe6.
    #[test]
    fn local_pricing_taints_the_total_as_estimated() {
        let usage = Usage {
            prompt_tokens: 1000,
            completion_tokens: 500,
            total_tokens: 1500,
            cached_tokens: 0,
        };

        let mut gateway = CostTracker::default();
        gateway.record_with_cost("groq-gpt-oss-120b", &usage, 0.0007);
        assert!(!gateway.cost_estimated, "a gateway-reported cost is authoritative");

        let mut local = CostTracker::default();
        local.record("groq-gpt-oss-120b", &usage, &ModelPricing::gpt_4o());
        assert!(local.cost_estimated, "the ModelPricing path must taint the total");

        // Taint is sticky: one estimated call in a multi-call turn makes the
        // whole total an estimate, which is the conservative reading.
        let mut mixed = CostTracker::default();
        mixed.record_with_cost("groq-gpt-oss-120b", &usage, 0.0007);
        mixed.record("groq-gpt-oss-120b", &usage, &ModelPricing::gpt_4o());
        mixed.record_with_cost("groq-gpt-oss-120b", &usage, 0.0007);
        assert!(mixed.cost_estimated, "one estimated call taints the turn");

        let mut reset = local.clone();
        reset.reset();
        assert!(!reset.cost_estimated, "reset must clear provenance too");
    }

    #[test]
    fn for_model_resolves_smooth_aliases() {
        // Pearl th-431ba2: every smooth-* alias used to fall to
        // the free tier, so cost stayed at $0 unless the gateway
        // returned a cost header.
        assert_ne!(
            ModelPricing::for_model("smooth-fast-gemini").prompt_per_mtok,
            0.0,
            "smooth-fast-gemini must map to gemini-flash pricing, not free tier",
        );
        assert_ne!(
            ModelPricing::for_model("smooth-coding").prompt_per_mtok,
            0.0,
            "smooth-coding must map to a real peer estimate, not free tier",
        );
        // Unknown model still hits the free tier.
        assert_eq!(ModelPricing::for_model("totally-fake-model-name-xyz").prompt_per_mtok, 0.0);
    }

    #[test]
    fn record_accumulates_tokens() {
        let mut tracker = CostTracker::default();
        let pricing = ModelPricing::gpt_4o();

        tracker.record(
            "gpt-4o",
            &Usage {
                prompt_tokens: 100,
                completion_tokens: 50,
                total_tokens: 150,
                ..Default::default()
            },
            &pricing,
        );
        tracker.record(
            "gpt-4o",
            &Usage {
                prompt_tokens: 200,
                completion_tokens: 100,
                total_tokens: 300,
                ..Default::default()
            },
            &pricing,
        );

        assert_eq!(tracker.total_prompt_tokens, 300);
        assert_eq!(tracker.total_completion_tokens, 150);
        assert_eq!(tracker.calls, 2);
        assert_eq!(tracker.entries().len(), 2);
    }

    #[test]
    fn cost_calculation_accuracy() {
        // 1000 prompt tokens at $3/Mtok = $0.003
        let pricing = ModelPricing {
            prompt_per_mtok: 3.0,
            completion_per_mtok: 0.0,
        };
        let mut tracker = CostTracker::default();
        tracker.record(
            "test-model",
            &Usage {
                prompt_tokens: 1000,
                completion_tokens: 0,
                total_tokens: 1000,
                ..Default::default()
            },
            &pricing,
        );

        let expected = 0.003;
        assert!(
            (tracker.total_cost_usd - expected).abs() < 1e-10,
            "expected {expected}, got {}",
            tracker.total_cost_usd
        );
    }

    #[test]
    fn check_budget_passes_when_under() {
        let mut tracker = CostTracker::default();
        tracker.record(
            "gpt-4o-mini",
            &Usage {
                prompt_tokens: 100,
                completion_tokens: 50,
                total_tokens: 150,
                ..Default::default()
            },
            &ModelPricing::gpt_4o_mini(),
        );

        let budget = CostBudget {
            max_cost_usd: Some(1.0),
            max_tokens: Some(1_000_000),
        };
        assert!(tracker.check_budget(&budget).is_ok());
    }

    #[test]
    fn check_budget_fails_on_usd_limit() {
        let mut tracker = CostTracker::default();
        // Use a pricing that makes 1000 tokens very expensive
        let pricing = ModelPricing {
            prompt_per_mtok: 1_000_000.0, // $1 per token
            completion_per_mtok: 0.0,
        };
        tracker.record(
            "expensive-model",
            &Usage {
                prompt_tokens: 100,
                completion_tokens: 0,
                total_tokens: 100,
                ..Default::default()
            },
            &pricing,
        );

        let budget = CostBudget {
            max_cost_usd: Some(1.0),
            max_tokens: None,
        };
        let err = tracker.check_budget(&budget).unwrap_err();
        assert!(err.spent_usd > 1.0);
        assert_eq!(err.limit_usd, Some(1.0));
    }

    #[test]
    fn check_budget_fails_on_token_limit() {
        let mut tracker = CostTracker::default();
        tracker.record(
            "gpt-4o",
            &Usage {
                prompt_tokens: 5000,
                completion_tokens: 5000,
                total_tokens: 10000,
                ..Default::default()
            },
            &ModelPricing::gpt_4o(),
        );

        let budget = CostBudget {
            max_cost_usd: None,
            max_tokens: Some(100),
        };
        let err = tracker.check_budget(&budget).unwrap_err();
        assert_eq!(err.total_tokens, 10000);
        assert_eq!(err.limit_tokens, Some(100));
    }

    #[test]
    fn model_pricing_presets_reasonable() {
        let gpt4o = ModelPricing::gpt_4o();
        assert!(gpt4o.prompt_per_mtok > 0.0);
        assert!(gpt4o.completion_per_mtok > gpt4o.prompt_per_mtok);

        let mini = ModelPricing::gpt_4o_mini();
        assert!(mini.prompt_per_mtok < gpt4o.prompt_per_mtok);
        assert!(mini.completion_per_mtok < gpt4o.completion_per_mtok);

        let free = ModelPricing::free();
        assert_eq!(free.prompt_per_mtok, 0.0);
        assert_eq!(free.completion_per_mtok, 0.0);

        let ds_v3 = ModelPricing::deepseek_v3();
        let ds_r1 = ModelPricing::deepseek_r1();
        assert!(ds_r1.prompt_per_mtok > ds_v3.prompt_per_mtok);

        let gemini = ModelPricing::gemini_flash();
        assert!(gemini.prompt_per_mtok > 0.0);
        assert!(gemini.prompt_per_mtok < mini.prompt_per_mtok);
    }

    #[test]
    fn cost_entry_timestamps_work() {
        let mut tracker = CostTracker::default();
        let before = Utc::now();

        tracker.record(
            "gpt-4o",
            &Usage {
                prompt_tokens: 10,
                completion_tokens: 10,
                total_tokens: 20,
                ..Default::default()
            },
            &ModelPricing::gpt_4o(),
        );

        let after = Utc::now();
        let entry = &tracker.entries()[0];
        assert!(entry.timestamp >= before);
        assert!(entry.timestamp <= after);
        assert_eq!(entry.model, "gpt-4o");
    }

    #[test]
    fn budget_exceeded_serialization() {
        let err = BudgetExceeded {
            spent_usd: 5.50,
            limit_usd: Some(5.0),
            total_tokens: 100_000,
            limit_tokens: Some(50_000),
        };

        let json = serde_json::to_string(&err).expect("serialize");
        assert!(json.contains("5.5"));
        assert!(json.contains("100000"));

        let deserialized: BudgetExceeded = serde_json::from_str(&json).expect("deserialize");
        assert!((deserialized.spent_usd - 5.50).abs() < f64::EPSILON);
        assert_eq!(deserialized.limit_usd, Some(5.0));
        assert_eq!(deserialized.total_tokens, 100_000);
        assert_eq!(deserialized.limit_tokens, Some(50_000));

        // Display impl
        let display = format!("{err}");
        assert!(display.contains("budget exceeded"));
        assert!(display.contains("5.5"));
    }
}
