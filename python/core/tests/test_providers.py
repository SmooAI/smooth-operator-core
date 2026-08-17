"""Provider-routing parity tests — the Python half of the cross-language contract.

The corpus tests are the drift gate: they replay ``spec/providers/routing.json``
(generated FROM the Rust reference) and assert this port resolves every preset slot
to the same model, base URL, key and wire format, matches the same quirks, builds
the same ``/model/info`` URLs, and parses the same alias maps. The rest port the
Rust engine's own unit tests — fallback chains, on-disk wire compatibility, env
loading, save/load round-trip.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from smooth_operator_core import (
    ALL_PRESETS,
    Activity,
    ApiFormat,
    ModelRouting,
    ModelSlot,
    Preset,
    ProviderConfig,
    ProviderRegistry,
    anthropic_provider,
    build_model_info_url,
    default_model_routing,
    google_provider,
    kimi_code_provider,
    kimi_provider,
    llmgateway_provider,
    ollama_provider,
    openai_provider,
    openrouter_provider,
    parse_model_info,
    preset_from_name,
    quirks_debug_snapshot,
    quirks_for_model,
    smooai_gateway_provider,
)

CORPUS_PATH = Path(__file__).resolve().parents[3] / "spec" / "providers" / "routing.json"
CORPUS = json.loads(CORPUS_PATH.read_text())

ACTIVITIES = {
    "coding": Activity.CODING,
    "reasoning": Activity.REASONING,
    "reviewing": Activity.REVIEWING,
    "judge": Activity.JUDGE,
    "summarize": Activity.SUMMARIZE,
    "fast": Activity.FAST,
}


@pytest.fixture(autouse=True)
def _clear_gateway_url(monkeypatch):
    """The corpus pins the production gateway URL, which only applies when
    SMOOAI_GATEWAY_URL is ABSENT."""
    monkeypatch.delenv("SMOOAI_GATEWAY_URL", raising=False)


def _resolve(registry: ProviderRegistry, label: str):
    return registry.default_llm_config() if label == "default" else registry.llm_config_for(ACTIVITIES[label])


def test_corpus_carries_all_five_presets():
    assert len(CORPUS["presets"]) == 5


@pytest.mark.parametrize("preset", CORPUS["presets"], ids=[p["name"] for p in CORPUS["presets"]])
def test_preset_routing_matches_corpus(preset):
    parsed = preset_from_name(preset["name"])
    assert parsed is not None
    assert parsed.provider_id == preset["providerId"]

    registry = ProviderRegistry.from_preset(parsed, "test-key")
    assert registry.list_providers() == preset["registeredProviders"]

    for label, want in preset["slots"].items():
        config = _resolve(registry, label)
        assert config.model == want["model"], label
        assert config.api_url == want["apiUrl"], label
        assert config.api_key == want["apiKey"], label
        assert config.api_format.value == want["apiFormat"], label
        assert config.max_tokens == want["maxTokens"], label
        assert config.temperature == want["temperature"], label


def test_preset_names_and_aliases_match_corpus():
    for vector in CORPUS["presetNames"]:
        parsed = preset_from_name(vector["name"])
        if vector["preset"] is None:
            assert parsed is None, vector["name"]
        else:
            assert parsed is not None and parsed.provider_id == vector["preset"], vector["name"]


def test_provider_factories_match_corpus():
    factories = {
        "openrouter": openrouter_provider("k"),
        "openai": openai_provider("k"),
        "anthropic": anthropic_provider("k"),
        "ollama": ollama_provider(),
        "google": google_provider("k"),
        "kimi": kimi_provider("k"),
        "kimiCode": kimi_code_provider("k"),
        "llmgateway": llmgateway_provider("k"),
        "smooaiGateway": smooai_gateway_provider("k"),
    }
    for want in CORPUS["providerFactories"]:
        got = factories[want["factory"]]
        assert got.id == want["id"], want["factory"]
        assert got.api_url == want["apiUrl"], want["factory"]
        assert got.api_key == want["apiKey"], want["factory"]
        assert got.api_format.value == want["apiFormat"], want["factory"]
        assert got.default_model == want["defaultModel"], want["factory"]


def test_default_routing_is_provider_agnostic():
    routing = default_model_routing()
    for label, want in CORPUS["defaultRouting"].items():
        slot = routing.default if label == "default" else routing.slot_for(ACTIVITIES[label])
        assert slot.provider == want["provider"], label
        assert slot.model == want["model"], label
    # The hosted gateway is opt-in, never the default.
    assert routing.coding.provider != "smooai-gateway"


@pytest.mark.parametrize("vector", CORPUS["wireCompat"], ids=[v["id"] for v in CORPUS["wireCompat"]])
def test_on_disk_wire_compat(vector):
    registry = ProviderRegistry.from_json(vector["json"])
    for label, want in vector["slotModels"].items():
        slot = registry.routing.default if label == "default" else registry.routing.slot_for(ACTIVITIES[label])
        assert slot.model == want, label


def test_fallback_chain_resolves_to_the_registered_provider():
    registry = ProviderRegistry()
    registry.register_provider(
        ProviderConfig("tertiary", "https://tertiary.example.com/v1", "t-key", ApiFormat.OPENAI_COMPAT, "model-c")
    )
    registry.routing.coding = ModelSlot("primary", "model-a").with_fallback(
        ModelSlot("secondary", "model-b").with_fallback(ModelSlot("tertiary", "model-c"))
    )

    config = registry.llm_config_for(Activity.CODING)
    want = CORPUS["fallbackChain"]
    assert config.api_url == want["apiUrl"]
    assert config.model == want["model"]
    assert config.api_key == want["apiKey"]


def test_unregistered_without_fallback_raises():
    assert CORPUS["unregisteredWithoutFallbackErrors"] is True
    registry = ProviderRegistry()
    registry.routing.coding = ModelSlot("nope", "m")
    with pytest.raises(ValueError, match="not registered"):
        registry.llm_config_for(Activity.CODING)


@pytest.mark.parametrize("vector", CORPUS["quirks"], ids=[v["upstream"] for v in CORPUS["quirks"]])
def test_quirks_match_corpus(vector):
    quirks = quirks_for_model(vector["upstream"])
    assert quirks.strict_tool_call_json is vector["strictToolCallJson"]
    assert quirks.allow_parallel_tools == vector["allowParallelTools"]
    assert sorted(quirks_debug_snapshot(vector["upstream"])) == vector["matchedKeys"]


def test_model_info_urls_match_corpus():
    for vector in CORPUS["modelInfoUrls"]:
        assert build_model_info_url(vector["apiUrl"]) == vector["modelInfoUrl"], vector["apiUrl"]


@pytest.mark.parametrize("vector", CORPUS["modelInfoParse"], ids=[v["id"] for v in CORPUS["modelInfoParse"]])
def test_model_info_parse_matches_corpus(vector):
    parsed = parse_model_info(vector["body"])
    assert list(parsed) == [e["alias"] for e in vector["entries"]]
    for want in vector["entries"]:
        entry = parsed[want["alias"]]
        assert entry.upstream == want["upstream"]
        assert entry.id == want["id"]


def test_parse_model_info_rejects_bad_bodies():
    with pytest.raises(ValueError, match="/model/info"):
        parse_model_info("not json")
    with pytest.raises(ValueError, match="data"):
        parse_model_info('{"nope": 1}')


def test_registry_writes_the_shape_rust_reads(tmp_path):
    path = tmp_path / "nested" / "providers.json"
    registry = ProviderRegistry()
    registry.register_provider(openrouter_provider("or-key"))
    registry.register_provider(openai_provider("oai-key"))
    registry.save_to_file(path)

    written = json.loads(path.read_text())
    assert sorted(written["providers"][0]) == ["api_format", "api_key", "api_url", "default_model", "id"]
    assert sorted(written["routing"]) == ["coding", "default", "fast", "judge", "reasoning", "reviewing", "summarize"]
    # `planning` is legacy: accepted on read, never written by a fresh config.
    assert "planning" not in written["routing"]
    # A slot with no fallback omits the key — `"fallback": null` is a different document.
    assert sorted(written["routing"]["coding"]) == ["model", "provider"]

    loaded = ProviderRegistry.load_from_file(path)
    assert loaded.list_providers() == ["openai", "openrouter"]
    assert loaded.get_provider("openrouter").api_key == "or-key"
    config = loaded.llm_config_for(Activity.REASONING)
    assert config.model == "openrouter/auto"
    assert config.api_key == "or-key"


def test_fallback_chain_survives_json_roundtrip():
    registry = ProviderRegistry.from_preset(Preset.OPENROUTER_LOW_COST, "k")
    restored = ProviderRegistry.from_json(registry.to_json())
    assert restored.routing.coding.fallback.model == "minimax/minimax-m2.5"
    assert restored.llm_config_for(Activity.CODING).model == "minimax/minimax-m2.7"


def test_from_env_reads_provider_and_model(monkeypatch):
    monkeypatch.setenv("SMOOTH_API_KEY", "env-test-key")
    monkeypatch.setenv("SMOOTH_PROVIDER", "openai")
    monkeypatch.delenv("SMOOTH_MODEL", raising=False)

    registry = ProviderRegistry.from_env()
    assert registry is not None
    assert registry.get_provider("openai").api_key == "env-test-key"
    assert registry.default_llm_config().model == "gpt-4o"

    monkeypatch.setenv("SMOOTH_MODEL", "gpt-4o-mini")
    assert ProviderRegistry.from_env().default_llm_config().model == "gpt-4o-mini"


def test_from_env_requires_a_key(monkeypatch):
    monkeypatch.delenv("SMOOTH_API_KEY", raising=False)
    assert ProviderRegistry.from_env() is None


def test_smooai_gateway_url_override(monkeypatch):
    monkeypatch.setenv("SMOOAI_GATEWAY_URL", "https://llm.dev.smooai.com/v1")
    config = ProviderRegistry.from_preset(Preset.SMOOAI_GATEWAY, "dev-key").default_llm_config()
    assert config.api_url == "https://llm.dev.smooai.com/v1"
    assert config.api_key == "dev-key"


def test_set_default_provider_then_remove():
    registry = ProviderRegistry()
    registry.register_provider(kimi_provider("k-key"))
    registry.set_default_provider("kimi")
    for activity in ACTIVITIES.values():
        config = registry.llm_config_for(activity)
        assert config.model == "kimi-k2.5"
        assert config.api_url == "https://api.moonshot.ai/v1"

    registry.remove_provider("kimi")
    with pytest.raises(ValueError):
        registry.llm_config_for(Activity.CODING)


def test_recommended_preset_is_listed_first():
    assert ALL_PRESETS[0].name == "smooai-gateway"
    assert "recommended" in ALL_PRESETS[0].label
    assert len(ALL_PRESETS) == 5


def test_client_for_refuses_a_non_openai_dialect():
    """The integration point: a resolved route becomes a live provider. An
    Anthropic-dialect provider must be refused, not spoken to in OpenAI's format."""
    client, config = ProviderRegistry.from_preset(Preset.OPENAI, "k").client_for(Activity.CODING)
    assert hasattr(client.chat.completions, "create")
    assert config.model == "gpt-4o"

    with pytest.raises(ValueError, match="cannot speak"):
        ProviderRegistry.from_preset(Preset.ANTHROPIC, "k").client_for(Activity.CODING)


def test_repr_never_leaks_the_api_key():
    assert "super-secret-key" not in repr(openrouter_provider("super-secret-key"))
    registry = ProviderRegistry.from_preset(Preset.OPENAI, "super-secret-key")
    assert "super-secret-key" not in repr(registry.llm_config_for(Activity.CODING))


def test_routing_slot_for_falls_back_to_default():
    """A partial table — no reasoning, no fast — still resolves both slots."""
    slot = ModelSlot("p", "m-default")
    routing = ModelRouting(coding=ModelSlot("p", "m-coding"), reviewing=slot, judge=slot, summarize=slot, default=slot)
    assert routing.slot_for(Activity.REASONING).model == "m-default"
    assert routing.slot_for(Activity.FAST).model == "m-default"
    assert routing.slot_for(Activity.CODING).model == "m-coding"
