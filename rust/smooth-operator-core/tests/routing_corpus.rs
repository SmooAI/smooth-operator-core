//! Provider-routing corpus — the Rust half of the cross-language drift gate.
//!
//! `spec/providers/routing.json` was generated FROM this engine, which is the
//! normative implementation of model routing, per-model quirks and LiteLLM alias
//! resolution: the Go, TypeScript, Python and .NET ports all replay the same file
//! and must resolve every preset slot to the same model, base URL, key and wire
//! format.
//!
//! That only holds if the reference is pinned too. Without this test a change to
//! `providers.rs` would move Rust and leave the other four engines silently
//! routing by the old table — and a routing divergence is the expensive kind: it
//! sends real traffic and real money somewhere nobody intended, and it looks like
//! it is working. So this asserts Rust still agrees with the file.

use std::collections::BTreeMap;

use smooth_operator_core::llm::ApiFormat;
use smooth_operator_core::providers::{Activity, ModelSlot, Preset, ProviderConfig, ProviderRegistry};
use smooth_operator_core::quirks;
use smooth_operator_core::resolution::{build_model_info_url, parse_model_info};

fn fmt_name(f: &ApiFormat) -> &'static str {
    match f {
        ApiFormat::OpenAiCompat => "OpenAiCompat",
        ApiFormat::Anthropic => "Anthropic",
    }
}

fn corpus() -> serde_json::Value {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../spec/providers/routing.json");
    let raw = std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("read {}: {e}", path.display()));
    serde_json::from_str(&raw).expect("corpus parses")
}

fn activity(label: &str) -> Option<Activity> {
    match label {
        "coding" => Some(Activity::Coding),
        "reasoning" => Some(Activity::Reasoning),
        "reviewing" => Some(Activity::Reviewing),
        "judge" => Some(Activity::Judge),
        "summarize" => Some(Activity::Summarize),
        "fast" => Some(Activity::Fast),
        _ => None,
    }
}

fn preset(name: &str) -> Preset {
    Preset::from_name(name).unwrap_or_else(|| panic!("preset {name} parses"))
}

#[test]
fn routing_matches_the_shared_corpus() {
    // The corpus pins the production gateway URL, which only applies when the
    // override is ABSENT.
    std::env::remove_var("SMOOAI_GATEWAY_URL");
    let c = corpus();

    // ── presets ────────────────────────────────────────────────────────────
    let presets = c["presets"].as_array().expect("presets");
    assert_eq!(presets.len(), 5, "corpus must carry all 5 presets");
    for p in presets {
        let name = p["name"].as_str().expect("name");
        let preset = preset(name);
        assert_eq!(preset.provider_id(), p["providerId"].as_str().unwrap(), "provider id for {name}");

        let registry = ProviderRegistry::from_preset(preset, "test-key");
        let registered: Vec<&str> = p["registeredProviders"].as_array().unwrap().iter().map(|v| v.as_str().unwrap()).collect();
        assert_eq!(registry.list_providers(), registered, "registered providers for {name}");

        for (label, want) in p["slots"].as_object().expect("slots") {
            let config = match activity(label) {
                Some(a) => registry.llm_config_for(a),
                None => registry.default_llm_config(),
            }
            .unwrap_or_else(|e| panic!("{name}/{label} resolves: {e}"));

            assert_eq!(config.model, want["model"].as_str().unwrap(), "{name}/{label} model");
            assert_eq!(config.api_url, want["apiUrl"].as_str().unwrap(), "{name}/{label} api_url");
            assert_eq!(config.api_key, want["apiKey"].as_str().unwrap(), "{name}/{label} api_key");
            assert_eq!(fmt_name(&config.api_format), want["apiFormat"].as_str().unwrap(), "{name}/{label} api_format");
            assert_eq!(u64::from(config.max_tokens), want["maxTokens"].as_u64().unwrap(), "{name}/{label} max_tokens");
            assert!(
                (f64::from(config.temperature) - want["temperature"].as_f64().unwrap()).abs() < f64::EPSILON,
                "{name}/{label} temperature"
            );
        }
    }

    // ── preset names + aliases ─────────────────────────────────────────────
    for v in c["presetNames"].as_array().expect("presetNames") {
        let name = v["name"].as_str().unwrap();
        match v["preset"].as_str() {
            Some(provider) => assert_eq!(
                Preset::from_name(name).map(|p| p.provider_id().to_string()),
                Some(provider.to_string()),
                "preset name {name}"
            ),
            None => assert!(Preset::from_name(name).is_none(), "preset name {name} must not resolve"),
        }
    }

    // ── provider factories ─────────────────────────────────────────────────
    for want in c["providerFactories"].as_array().expect("providerFactories") {
        let factory = want["factory"].as_str().unwrap();
        let got = match factory {
            "openrouter" => ProviderConfig::openrouter("k"),
            "openai" => ProviderConfig::openai("k"),
            "anthropic" => ProviderConfig::anthropic("k"),
            "ollama" => ProviderConfig::ollama(),
            "google" => ProviderConfig::google("k"),
            "kimi" => ProviderConfig::kimi("k"),
            "kimiCode" => ProviderConfig::kimi_code("k"),
            "llmgateway" => ProviderConfig::llmgateway("k"),
            "smooaiGateway" => ProviderConfig::smooai_gateway("k"),
            other => panic!("no Rust factory for {other}"),
        };
        assert_eq!(got.id, want["id"].as_str().unwrap(), "{factory} id");
        assert_eq!(got.api_url, want["apiUrl"].as_str().unwrap(), "{factory} api_url");
        assert_eq!(got.api_key, want["apiKey"].as_str().unwrap(), "{factory} api_key");
        assert_eq!(fmt_name(&got.api_format), want["apiFormat"].as_str().unwrap(), "{factory} api_format");
        assert_eq!(got.default_model, want["defaultModel"].as_str().unwrap(), "{factory} default_model");
    }

    // ── neutral default routing ────────────────────────────────────────────
    let registry = ProviderRegistry::new();
    for (label, want) in c["defaultRouting"].as_object().expect("defaultRouting") {
        let slot = match activity(label) {
            Some(a) => registry.routing.slot_for(a),
            None => &registry.routing.default,
        };
        assert_eq!(slot.provider, want["provider"].as_str().unwrap(), "default routing {label} provider");
        assert_eq!(slot.model, want["model"].as_str().unwrap(), "default routing {label} model");
    }
    assert_ne!(
        registry.routing.coding.provider, "smooai-gateway",
        "the hosted gateway is opt-in, never the default"
    );

    // ── on-disk wire compatibility ─────────────────────────────────────────
    for v in c["wireCompat"].as_array().expect("wireCompat") {
        let id = v["id"].as_str().unwrap();
        let loaded = ProviderRegistry::from_json(v["json"].as_str().unwrap()).unwrap_or_else(|e| panic!("{id} parses: {e}"));
        for (label, want) in v["slotModels"].as_object().unwrap() {
            let slot = match activity(label) {
                Some(a) => loaded.routing.slot_for(a),
                None => &loaded.routing.default,
            };
            assert_eq!(slot.model, want.as_str().unwrap(), "{id}/{label}");
        }
    }

    // ── fallback chain + hard failure ──────────────────────────────────────
    let mut chained = ProviderRegistry::new();
    chained.register_provider(ProviderConfig {
        id: "tertiary".into(),
        api_url: "https://tertiary.example.com/v1".into(),
        api_key: "t-key".into(),
        api_format: ApiFormat::OpenAiCompat,
        default_model: "model-c".into(),
    });
    chained.routing.coding =
        ModelSlot::new("primary", "model-a").with_fallback(ModelSlot::new("secondary", "model-b").with_fallback(ModelSlot::new("tertiary", "model-c")));
    let config = chained.llm_config_for(Activity::Coding).expect("chain resolves");
    let want_chain = &c["fallbackChain"];
    assert_eq!(config.api_url, want_chain["apiUrl"].as_str().unwrap());
    assert_eq!(config.model, want_chain["model"].as_str().unwrap());
    assert_eq!(config.api_key, want_chain["apiKey"].as_str().unwrap());

    let mut bare = ProviderRegistry::new();
    bare.routing.coding = ModelSlot::new("nope", "m");
    assert_eq!(
        bare.llm_config_for(Activity::Coding).is_err(),
        c["unregisteredWithoutFallbackErrors"].as_bool().unwrap(),
        "an unregistered provider with no fallback must be an error, not a silent substitution"
    );

    // ── quirks ─────────────────────────────────────────────────────────────
    for v in c["quirks"].as_array().expect("quirks") {
        let upstream = v["upstream"].as_str().unwrap();
        let q = quirks::for_model(upstream);
        assert_eq!(
            q.strict_tool_call_json,
            v["strictToolCallJson"].as_bool().unwrap(),
            "{upstream} strict_tool_call_json"
        );
        assert_eq!(q.allow_parallel_tools, v["allowParallelTools"].as_bool(), "{upstream} allow_parallel_tools");
        let mut keys: Vec<String> = quirks::debug_snapshot(upstream).into_keys().collect();
        keys.sort();
        let want_keys: Vec<String> = v["matchedKeys"].as_array().unwrap().iter().map(|k| k.as_str().unwrap().to_string()).collect();
        assert_eq!(keys, want_keys, "{upstream} matched keys");
    }

    // ── /model/info URL + parse ────────────────────────────────────────────
    for v in c["modelInfoUrls"].as_array().expect("modelInfoUrls") {
        let api_url = v["apiUrl"].as_str().unwrap();
        assert_eq!(build_model_info_url(api_url), v["modelInfoUrl"].as_str().unwrap(), "url for {api_url}");
    }

    for v in c["modelInfoParse"].as_array().expect("modelInfoParse") {
        let id = v["id"].as_str().unwrap();
        let parsed: BTreeMap<_, _> = parse_model_info(v["body"].as_str().unwrap()).unwrap_or_else(|e| panic!("{id} parses: {e}"));
        let entries = v["entries"].as_array().unwrap();
        let got_aliases: Vec<&str> = parsed.keys().map(String::as_str).collect();
        let want_aliases: Vec<&str> = entries.iter().map(|e| e["alias"].as_str().unwrap()).collect();
        assert_eq!(got_aliases, want_aliases, "{id} alias order");
        for want in entries {
            let alias = want["alias"].as_str().unwrap();
            let entry = parsed.get(alias).unwrap();
            assert_eq!(entry.upstream.as_deref(), want["upstream"].as_str(), "{id}/{alias} upstream");
            assert_eq!(entry.id.as_deref(), want["id"].as_str(), "{id}/{alias} id");
        }
    }
}
