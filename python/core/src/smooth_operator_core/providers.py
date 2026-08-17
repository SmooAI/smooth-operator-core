"""Provider routing — the Python port of the Rust reference engine's ``providers.rs``,
``quirks.rs`` and ``resolution.rs``.

Three concerns, one module because they are one story: **which** model a given
activity should use, **what** wire quirks that concrete model has, and — when the
route points at a LiteLLM-style gateway — **which** upstream model a semantic
alias actually resolves to.

- :class:`ProviderRegistry` holds provider credentials/URLs and a
  :class:`ModelRouting` table mapping each :class:`Activity` to a
  :class:`ModelSlot`. :meth:`ProviderRegistry.llm_config_for` walks the slot's
  fallback chain until it finds a registered provider.
- :func:`quirks_for_model` looks up per-model wire quirks by substring on the
  concrete upstream name.
- :func:`build_model_info_url` / :func:`parse_model_info` / :func:`fetch_model_info`
  recover the gateway's alias → upstream map from ``GET /model/info``.

The on-disk JSON shape is shared with the Rust CLI (``~/.smooth/providers.json``),
so the serialized keys are snake_case and must stay byte-compatible: the same file
is written by one engine and read by another. Legacy ``thinking`` / ``planning``
field names still deserialize onto the merged ``reasoning`` slot.

Routing values are pinned across all five engines by the shared corpus at
``spec/providers/routing.json`` — a slot that resolves to the wrong model or base
URL sends real traffic and real money somewhere nobody intended, and it looks like
it is working.
"""

from __future__ import annotations

import asyncio
import json
import os
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path
from typing import Any

from .gateway_client import GatewayLlmProvider

# ---------------------------------------------------------------------------
# Providers
# ---------------------------------------------------------------------------


class ApiFormat(str, Enum):
    """The wire dialect a provider speaks. The values match the Rust reference's
    serde output so ``providers.json`` round-trips between engines."""

    #: The OpenAI ``/chat/completions`` dialect.
    OPENAI_COMPAT = "OpenAiCompat"
    #: Anthropic's native ``/messages`` dialect.
    ANTHROPIC = "Anthropic"


@dataclass
class ProviderConfig:
    """Connection detail for a single LLM provider."""

    id: str
    api_url: str
    api_key: str
    api_format: ApiFormat
    default_model: str

    def __repr__(self) -> str:
        """Redact the API key so it never lands in logs, tracebacks or error
        chains. Everything else is printed verbatim, mirroring the Rust
        reference's manual ``Debug`` impl."""
        return (
            f"ProviderConfig(id={self.id!r}, api_url={self.api_url!r}, api_key='***redacted***', "
            f"api_format={self.api_format.value!r}, default_model={self.default_model!r})"
        )


def openrouter_provider(api_key: str) -> ProviderConfig:
    """OpenRouter — an OpenAI-compatible proxy for many models."""
    return ProviderConfig(
        "openrouter", "https://openrouter.ai/api/v1", api_key, ApiFormat.OPENAI_COMPAT, "openai/gpt-4o"
    )


def openai_provider(api_key: str) -> ProviderConfig:
    """The OpenAI direct API."""
    return ProviderConfig("openai", "https://api.openai.com/v1", api_key, ApiFormat.OPENAI_COMPAT, "gpt-4o")


def anthropic_provider(api_key: str) -> ProviderConfig:
    """The Anthropic native API."""
    return ProviderConfig(
        "anthropic", "https://api.anthropic.com/v1", api_key, ApiFormat.ANTHROPIC, "claude-sonnet-4-20250514"
    )


def ollama_provider() -> ProviderConfig:
    """A local Ollama instance — no API key needed."""
    return ProviderConfig("ollama", "http://localhost:11434/v1", "", ApiFormat.OPENAI_COMPAT, "llama3")


def google_provider(api_key: str) -> ProviderConfig:
    """The Google Gemini API (OpenAI-compatible surface)."""
    return ProviderConfig(
        "google",
        "https://generativelanguage.googleapis.com/v1beta/openai",
        api_key,
        ApiFormat.OPENAI_COMPAT,
        "gemini-2.0-flash",
    )


def kimi_provider(api_key: str) -> ProviderConfig:
    """Moonshot AI's general-purpose API (OpenAI-compatible)."""
    return ProviderConfig("kimi", "https://api.moonshot.ai/v1", api_key, ApiFormat.OPENAI_COMPAT, "kimi-k2.5")


def kimi_code_provider(api_key: str) -> ProviderConfig:
    """Moonshot's coding-optimized API (Anthropic-compatible)."""
    return ProviderConfig(
        "kimi-code", "https://api.kimi.com/coding/v1", api_key, ApiFormat.ANTHROPIC, "kimi-for-coding"
    )


def llmgateway_provider(api_key: str) -> ProviderConfig:
    """LLM Gateway — a unified API for 210+ models."""
    return ProviderConfig(
        "llmgateway", "https://api.llmgateway.io/v1", api_key, ApiFormat.OPENAI_COMPAT, "openai/gpt-4o"
    )


def smooai_gateway_provider(api_key: str) -> ProviderConfig:
    """The hosted LiteLLM-backed gateway run by Smoo AI.

    One API key, one URL, OpenAI-compatible. The gateway handles provider
    selection, billing, moderation and cost tracking server-side, so consumers
    reference models by semantic aliases (``smooth-coding``, ``smooth-judge``, …)
    that the gateway maps to whichever underlying model is currently best —
    upgrades ship server-side with no client release.

    ``SMOOAI_GATEWAY_URL`` overrides the base URL. Only an ABSENT variable takes
    the default: a set-but-empty override yields an empty base URL, matching Rust.
    """
    override = os.environ.get("SMOOAI_GATEWAY_URL")
    api_url = "https://llm.smoo.ai/v1" if override is None else override
    return ProviderConfig("smooai-gateway", api_url, api_key, ApiFormat.OPENAI_COMPAT, "smooth-default")


# ---------------------------------------------------------------------------
# Presets
# ---------------------------------------------------------------------------


class Preset(str, Enum):
    """A ready-made provider + routing configuration."""

    #: The hosted Smoo AI gateway — the recommended default.
    SMOOAI_GATEWAY = "SmoaiGateway"
    #: Chinese frontier models via OpenRouter — the cheapest option.
    OPENROUTER_LOW_COST = "OpenRouterLowCost"
    #: Chinese frontier models via LLM Gateway.
    LLMGATEWAY_LOW_COST = "LlmGatewayLowCost"
    #: OpenAI models.
    OPENAI = "OpenAI"
    #: Anthropic Claude models.
    ANTHROPIC = "Anthropic"

    @property
    def provider_id(self) -> str:
        """The provider this preset requires."""
        return {
            Preset.SMOOAI_GATEWAY: "smooai-gateway",
            Preset.OPENROUTER_LOW_COST: "openrouter",
            Preset.LLMGATEWAY_LOW_COST: "llmgateway",
            Preset.OPENAI: "openai",
            Preset.ANTHROPIC: "anthropic",
        }[self]


@dataclass(frozen=True)
class PresetInfo:
    """One row of :data:`ALL_PRESETS`: CLI name, display label, description."""

    name: str
    label: str
    description: str


#: Every preset. The first entry is the recommended default — ``th auth login``
#: shows them in this order.
ALL_PRESETS: list[PresetInfo] = [
    PresetInfo(
        "smooai-gateway",
        "Smoo AI Gateway (recommended)",
        "Hosted LiteLLM gateway run by Smoo AI — billing, moderation, governance, 100+ models. One key, one URL, no config.",
    ),
    PresetInfo(
        "openrouter-low-cost",
        "OpenRouter Low Cost",
        "GLM-5.1 thinking (#1 SWE-Bench Pro), MiniMax-M2.7 coding (56% SWE-Pro, 10B params), DeepSeek-V3.2 default",
    ),
    PresetInfo(
        "llmgateway-low-cost",
        "LLM Gateway Low Cost",
        "GLM-5 thinking, MiniMax-M2.7 coding, DeepSeek-V3.2 default — unified billing, 224 models",
    ),
    PresetInfo("openai", "OpenAI", "o3-mini thinking, GPT-4o coding — OpenAI ecosystem"),
    PresetInfo("anthropic", "Anthropic", "Claude Opus thinking, Sonnet coding — highest quality"),
]

_PRESET_NAMES = {
    "smooai-gateway": Preset.SMOOAI_GATEWAY,
    "smooai": Preset.SMOOAI_GATEWAY,
    "gateway": Preset.SMOOAI_GATEWAY,
    "openrouter-low-cost": Preset.OPENROUTER_LOW_COST,
    "low-cost": Preset.OPENROUTER_LOW_COST,
    "llmgateway-low-cost": Preset.LLMGATEWAY_LOW_COST,
    "gateway-low-cost": Preset.LLMGATEWAY_LOW_COST,
    "openai": Preset.OPENAI,
    "codex": Preset.OPENAI,
    "anthropic": Preset.ANTHROPIC,
}


def preset_from_name(name: str) -> Preset | None:
    """Parse a preset name or alias. Returns ``None`` for unknown names."""
    return _PRESET_NAMES.get(name)


# ---------------------------------------------------------------------------
# Routing
# ---------------------------------------------------------------------------


class Activity(str, Enum):
    """Selects which model slot a call routes through. Six semantic slots: the
    legacy ``Thinking`` + ``Planning`` split collapsed into :attr:`REASONING`, and
    the legacy "default" alias is served by :attr:`CODING`."""

    #: The outer coding loop — the workhorse slot, which also serves the legacy
    #: "default" call path.
    CODING = "Coding"
    #: Deep reasoning / planning / chain-of-thought.
    REASONING = "Reasoning"
    #: Code review, critique, adversarial checks.
    REVIEWING = "Reviewing"
    #: LLM-as-a-judge: yes/no verdicts, low latency, used by Narc guardrails and
    #: bench scoring.
    JUDGE = "Judge"
    #: Context compression during long agent runs.
    SUMMARIZE = "Summarize"
    #: Small, latency-sensitive utility calls: session auto-naming, short titles,
    #: autocomplete. Sub-second first token, short output, no tool use — don't pay
    #: Sonnet-plus prices to name a session.
    FAST = "Fast"


@dataclass
class ModelSlot:
    """A provider id + model name, with an optional fallback used when the
    provider is not registered."""

    provider: str
    model: str
    fallback: ModelSlot | None = None

    def with_fallback(self, fallback: ModelSlot) -> ModelSlot:
        """Return a copy of this slot with ``fallback`` attached."""
        return ModelSlot(self.provider, self.model, fallback)

    def to_wire(self) -> dict[str, Any]:
        """The on-disk dict. A slot with no fallback OMITS the key entirely —
        ``"fallback": null`` is a different document to what Rust writes."""
        wire: dict[str, Any] = {"provider": self.provider, "model": self.model}
        if self.fallback is not None:
            wire["fallback"] = self.fallback.to_wire()
        return wire

    @staticmethod
    def from_wire(wire: dict[str, Any]) -> ModelSlot:
        """Build a slot from its on-disk dict."""
        fallback = wire.get("fallback")
        return ModelSlot(wire["provider"], wire["model"], ModelSlot.from_wire(fallback) if fallback else None)


@dataclass
class ModelRouting:
    """The per-activity routing table.

    Six semantic slots plus a ``default`` slot kept for wire compatibility: no
    :class:`Activity` routes through ``default`` directly (:attr:`Activity.CODING`
    serves the default path), but the field stays so pre-collapse configs load
    cleanly.
    """

    coding: ModelSlot
    reviewing: ModelSlot
    judge: ModelSlot
    summarize: ModelSlot
    default: ModelSlot
    #: Merged deep-reasoning slot. Absent in older files (which carry
    #: ``thinking``); falls back to ``default`` at lookup time.
    reasoning: ModelSlot | None = None
    #: Optional on disk: pre-``fast`` files fall back to ``default``.
    fast: ModelSlot | None = None
    #: Legacy field, deserialized but ignored at lookup time — ``reasoning``
    #: absorbed the planning slot.
    planning: ModelSlot | None = None

    def slot_for(self, activity: Activity) -> ModelSlot:
        """The slot for an activity. ``REASONING`` and ``FAST`` fall back to
        ``default`` when absent, so partial configs stay functional."""
        if activity is Activity.CODING:
            return self.coding
        if activity is Activity.REASONING:
            return self.reasoning if self.reasoning is not None else self.default
        if activity is Activity.REVIEWING:
            return self.reviewing
        if activity is Activity.JUDGE:
            return self.judge
        if activity is Activity.SUMMARIZE:
            return self.summarize
        if activity is Activity.FAST:
            return self.fast if self.fast is not None else self.default
        return self.default

    def to_wire(self) -> dict[str, Any]:
        """The on-disk dict, snake_case keys and all. Optional slots are omitted
        when unset, matching serde's ``skip_serializing_if``."""
        wire: dict[str, Any] = {"coding": self.coding.to_wire()}
        if self.reasoning is not None:
            wire["reasoning"] = self.reasoning.to_wire()
        wire["reviewing"] = self.reviewing.to_wire()
        wire["judge"] = self.judge.to_wire()
        wire["summarize"] = self.summarize.to_wire()
        wire["default"] = self.default.to_wire()
        if self.fast is not None:
            wire["fast"] = self.fast.to_wire()
        if self.planning is not None:
            wire["planning"] = self.planning.to_wire()
        return wire

    @staticmethod
    def from_wire(wire: dict[str, Any]) -> ModelRouting:
        """Build routing from its on-disk dict, migrating the legacy ``thinking``
        field onto :attr:`reasoning`. An explicit ``reasoning`` always wins."""
        reasoning = wire.get("reasoning") or wire.get("thinking")
        optional = {key: ModelSlot.from_wire(wire[key]) for key in ("fast", "planning") if wire.get(key)}
        return ModelRouting(
            coding=ModelSlot.from_wire(wire["coding"]),
            reviewing=ModelSlot.from_wire(wire["reviewing"]),
            judge=ModelSlot.from_wire(wire["judge"]),
            summarize=ModelSlot.from_wire(wire["summarize"]),
            default=ModelSlot.from_wire(wire["default"]),
            reasoning=ModelSlot.from_wire(reasoning) if reasoning else None,
            **optional,
        )


def default_model_routing() -> ModelRouting:
    """The neutral, provider-agnostic routing every slot starts on: the well-known
    ``openrouter`` provider id with a placeholder ``auto`` model, so the library
    ships no opinion about a specific hosted gateway. Consumers opt into the Smoo
    AI gateway via :attr:`Preset.SMOOAI_GATEWAY` explicitly."""
    return _uniform_routing(ModelSlot("openrouter", "openrouter/auto"))


def _uniform_routing(slot: ModelSlot) -> ModelRouting:
    return ModelRouting(
        coding=slot,
        reviewing=slot,
        judge=slot,
        summarize=slot,
        default=slot,
        reasoning=slot,
        fast=slot,
    )


# ---------------------------------------------------------------------------
# Resolved config
# ---------------------------------------------------------------------------


@dataclass
class LlmConfig:
    """A fully resolved route: the provider connection plus the model the activity
    picked. Feed ``api_url``/``api_key`` to :class:`GatewayLlmProvider`."""

    api_url: str
    api_key: str
    model: str
    max_tokens: int = 32768
    temperature: float = 0.0
    api_format: ApiFormat = ApiFormat.OPENAI_COMPAT

    def __repr__(self) -> str:
        """Redact the API key, same as :meth:`ProviderConfig.__repr__`."""
        return (
            f"LlmConfig(api_url={self.api_url!r}, api_key='***redacted***', model={self.model!r}, "
            f"max_tokens={self.max_tokens!r}, temperature={self.temperature!r}, api_format={self.api_format.value!r})"
        )


# ---------------------------------------------------------------------------
# Registry
# ---------------------------------------------------------------------------


@dataclass
class ProviderRegistry:
    """Registered providers plus the per-activity routing table."""

    routing: ModelRouting = field(default_factory=default_model_routing)
    _providers: dict[str, ProviderConfig] = field(default_factory=dict, repr=False)

    @staticmethod
    def from_preset(preset: Preset, api_key: str) -> ProviderRegistry:
        """A registry pre-configured with a preset: registers the preset's provider
        and installs routing tuned for the preset's goals (cost, quality, latency)."""
        registry = ProviderRegistry()
        slot = ModelSlot

        if preset is Preset.SMOOAI_GATEWAY:
            # Semantic aliases the gateway's LiteLLM config maps to whichever
            # underlying model is currently best. Changing the underlying model is
            # a server-side deploy — no client release needed.
            registry.register_provider(smooai_gateway_provider(api_key))
            registry.routing = ModelRouting(
                coding=slot("smooai-gateway", "smooth-coding"),
                reasoning=slot("smooai-gateway", "smooth-reasoning"),
                reviewing=slot("smooai-gateway", "smooth-reviewing"),
                judge=slot("smooai-gateway", "smooth-judge"),
                summarize=slot("smooai-gateway", "smooth-summarize"),
                default=slot("smooai-gateway", "smooth-default"),
                fast=slot("smooai-gateway", "smooth-fast"),
            )
        elif preset is Preset.OPENROUTER_LOW_COST:
            # OpenRouter uses provider-prefixed model IDs.
            registry.register_provider(openrouter_provider(api_key))
            registry.routing = ModelRouting(
                coding=slot("openrouter", "minimax/minimax-m2.7").with_fallback(
                    slot("openrouter", "minimax/minimax-m2.5")
                ),
                reasoning=slot("openrouter", "z-ai/glm-5.1"),
                reviewing=slot("openrouter", "deepseek/deepseek-v3.2"),
                judge=slot("openrouter", "google/gemini-2.5-flash"),
                summarize=slot("openrouter", "deepseek/deepseek-v3.2"),
                default=slot("openrouter", "deepseek/deepseek-v3.2"),
                fast=slot("openrouter", "google/gemini-2.5-flash-lite"),
            )
        elif preset is Preset.LLMGATEWAY_LOW_COST:
            # LLM Gateway uses bare model names.
            registry.register_provider(llmgateway_provider(api_key))
            registry.routing = ModelRouting(
                coding=slot("llmgateway", "minimax-m2.7").with_fallback(slot("llmgateway", "minimax-m2.5")),
                reasoning=slot("llmgateway", "glm-5"),
                reviewing=slot("llmgateway", "deepseek-v3.2"),
                judge=slot("llmgateway", "gemini-2.5-flash"),
                summarize=slot("llmgateway", "deepseek-v3.2"),
                default=slot("llmgateway", "deepseek-v3.2"),
                fast=slot("llmgateway", "gemini-2.5-flash-lite"),
            )
        elif preset is Preset.OPENAI:
            registry.register_provider(openai_provider(api_key))
            registry.routing = ModelRouting(
                coding=slot("openai", "gpt-4o"),
                reasoning=slot("openai", "o3-mini"),
                reviewing=slot("openai", "gpt-4o"),
                judge=slot("openai", "gpt-4o-mini"),
                summarize=slot("openai", "gpt-4o-mini"),
                default=slot("openai", "gpt-4o"),
                fast=slot("openai", "gpt-4o-mini"),
            )
        elif preset is Preset.ANTHROPIC:
            registry.register_provider(anthropic_provider(api_key))
            registry.routing = ModelRouting(
                coding=slot("anthropic", "claude-sonnet-4-20250514"),
                reasoning=slot("anthropic", "claude-opus-4-20250514"),
                reviewing=slot("anthropic", "claude-sonnet-4-20250514"),
                judge=slot("anthropic", "claude-haiku-4-5-20251001"),
                summarize=slot("anthropic", "claude-haiku-4-5-20251001"),
                default=slot("anthropic", "claude-sonnet-4-20250514"),
                fast=slot("anthropic", "claude-haiku-4-5-20251001"),
            )
        return registry

    @staticmethod
    def from_env() -> ProviderRegistry | None:
        """A minimal registry from ``SMOOTH_API_KEY`` (required),
        ``SMOOTH_PROVIDER`` (defaults to ``openrouter``) and ``SMOOTH_MODEL``
        (optional). Returns ``None`` when ``SMOOTH_API_KEY`` is unset — never a
        keyless client."""
        api_key = os.environ.get("SMOOTH_API_KEY")
        if api_key is None:
            return None
        provider_id = os.environ.get("SMOOTH_PROVIDER") or "openrouter"

        builders = {
            "openai": openai_provider,
            "anthropic": anthropic_provider,
            "google": google_provider,
            "kimi": kimi_provider,
            "kimi-code": kimi_code_provider,
            "llmgateway": llmgateway_provider,
        }
        if provider_id == "ollama":
            config = ollama_provider()
            config.api_key = api_key
        else:
            config = builders.get(provider_id, openrouter_provider)(api_key)

        registry = ProviderRegistry()
        registry.register_provider(config)
        registry.routing = _uniform_routing(
            ModelSlot(provider_id, os.environ.get("SMOOTH_MODEL") or config.default_model)
        )
        return registry

    @staticmethod
    def from_json(data: str) -> ProviderRegistry:
        """Deserialize a registry from the JSON shape :meth:`to_json` writes."""
        file = json.loads(data)
        registry = ProviderRegistry(routing=ModelRouting.from_wire(file["routing"]))
        for entry in file.get("providers") or []:
            registry.register_provider(
                ProviderConfig(
                    entry["id"],
                    entry["api_url"],
                    entry["api_key"],
                    ApiFormat(entry["api_format"]),
                    entry["default_model"],
                )
            )
        return registry

    @staticmethod
    def load_from_file(path: str | Path) -> ProviderRegistry:
        """Read a registry from a JSON file (e.g. ``~/.smooth/providers.json``)."""
        return ProviderRegistry.from_json(Path(path).read_text())

    def register_provider(self, config: ProviderConfig) -> None:
        """Add (or replace) a provider configuration."""
        self._providers[config.id] = config

    def remove_provider(self, provider_id: str) -> None:
        """Drop a provider by id."""
        self._providers.pop(provider_id, None)

    def get_provider(self, provider_id: str) -> ProviderConfig | None:
        """Look up a provider by id."""
        return self._providers.get(provider_id)

    def list_providers(self) -> list[str]:
        """Every registered provider id, sorted."""
        return sorted(self._providers)

    def set_default_provider(self, provider_id: str) -> None:
        """Point every routing slot at ``provider_id`` using its default model."""
        provider = self._providers.get(provider_id)
        self.routing = _uniform_routing(ModelSlot(provider_id, provider.default_model if provider else ""))

    def with_routing(self, routing: ModelRouting) -> ProviderRegistry:
        """Install a custom routing table."""
        self.routing = routing
        return self

    def _resolve_slot(self, slot: ModelSlot) -> LlmConfig:
        provider = self._providers.get(slot.provider)
        if provider is not None:
            return LlmConfig(
                api_url=provider.api_url,
                api_key=provider.api_key,
                model=slot.model,
                max_tokens=32768,
                temperature=0.0,
                api_format=provider.api_format,
            )
        if slot.fallback is not None:
            return self._resolve_slot(slot.fallback)
        raise ValueError(f"provider '{slot.provider}' not registered and no fallback available")

    def llm_config_for(self, activity: Activity) -> LlmConfig:
        """Resolve the route for an activity.

        Raises :class:`ValueError` when the slot's provider — and every fallback —
        is unregistered, rather than silently substituting some other provider."""
        return self._resolve_slot(self.routing.slot_for(activity))

    def default_llm_config(self) -> LlmConfig:
        """Resolve the wire-compat ``default`` slot."""
        return self._resolve_slot(self.routing.default)

    def client_for(self, activity: Activity) -> tuple[GatewayLlmProvider, LlmConfig]:
        """Build a gateway provider for an activity's resolved route — the one line
        between "which model should this call use" and a client that speaks to it.

        The client is OpenAI-compatible; an :attr:`ApiFormat.ANTHROPIC` provider is
        rejected rather than silently spoken to in the wrong dialect."""
        config = self.llm_config_for(activity)
        if config.api_format is not ApiFormat.OPENAI_COMPAT:
            raise ValueError(
                f"activity {activity.value} routes to a {config.api_format.value} provider, "
                "which the OpenAI-compatible gateway client cannot speak"
            )
        return GatewayLlmProvider(base_url=config.api_url, api_key=config.api_key), config

    def to_json(self, pretty: bool = False) -> str:
        """Serialize to the on-disk JSON shape, snake_case keys and all."""
        file = {
            "providers": [
                {
                    "id": p.id,
                    "api_url": p.api_url,
                    "api_key": p.api_key,
                    "api_format": p.api_format.value,
                    "default_model": p.default_model,
                }
                for p in (self._providers[i] for i in self.list_providers())
            ],
            "routing": self.routing.to_wire(),
        }
        return json.dumps(file, indent=2) if pretty else json.dumps(file)

    def save_to_file(self, path: str | Path) -> None:
        """Write the registry as pretty-printed JSON, creating parent directories."""
        target = Path(path)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(self.to_json(pretty=True))


# ---------------------------------------------------------------------------
# Per-model wire quirks
# ---------------------------------------------------------------------------


@dataclass
class ModelQuirks:
    """Per-model wire-format flags. Populate a field only when the quirk is worth
    the branch — every conditional is a place for drift.

    When routing through a LiteLLM-style gateway the concrete upstream model only
    reveals itself in response headers (``x-litellm-model-name``), by which point
    the request is already sent. So prefer always-safe request shapes over
    per-model conditionals, and keep this table for the cases where the strict
    form does not work everywhere.
    """

    #: When not ``None`` and false, force ``parallel_tool_calls`` off even if the
    #: agent config requests it.
    allow_parallel_tools: bool | None = None
    #: Ask the client to be extra careful about tool_call echo shape. Nothing reads
    #: this yet; it is the anchor for future defensive tweaks.
    strict_tool_call_json: bool = False


_QUIRKS_TABLE: list[tuple[str, ModelQuirks]] = [
    ("qwen3-coder", ModelQuirks(strict_tool_call_json=True)),
    ("qwen-coder", ModelQuirks(strict_tool_call_json=True)),
]


def quirks_for_model(upstream: str) -> ModelQuirks:
    """Look up quirks by concrete upstream name. Matching is case-insensitive and
    substring-based, so minor version drift (``qwen3-coder-plus-2025-04``) still
    hits the ``qwen3-coder`` entry. Returns safe defaults when nothing matches."""
    lowered = upstream.lower()
    for needle, quirks in _QUIRKS_TABLE:
        if needle in lowered:
            return ModelQuirks(quirks.allow_parallel_tools, quirks.strict_tool_call_json)
    return ModelQuirks()


def quirk_keys() -> list[str]:
    """The quirk table's canonical keys, for diagnostics."""
    return [needle for needle, _ in _QUIRKS_TABLE]


def quirks_debug_snapshot(upstream: str) -> dict[str, ModelQuirks]:
    """Every quirk entry matching an upstream name. Usually one wins; the full set
    is kept so tests can assert coverage."""
    lowered = upstream.lower()
    return {needle: quirks for needle, quirks in _QUIRKS_TABLE if needle in lowered}


# ---------------------------------------------------------------------------
# LiteLLM alias resolution
# ---------------------------------------------------------------------------


@dataclass
class ResolvedModel:
    """One routing entry returned by a gateway's ``/model/info``."""

    #: The name callers use (e.g. ``smooth-coding``).
    alias: str
    #: The concrete model (e.g. ``moonshot/kimi-k2-thinking``), when the gateway
    #: chose to surface it.
    upstream: str | None = None
    #: Stable id from ``model_info.id``, useful for tracing a rename.
    id: str | None = None


def build_model_info_url(api_url: str) -> str:
    """Derive the ``/model/info`` URL from a provider's OpenAI-compat ``api_url``
    (e.g. ``https://llm.smoo.ai/v1``). Stripping ``/v1`` is safe: ``/model/info``
    lives at the gateway root in every LiteLLM deployment seen."""
    trimmed = api_url.rstrip("/")
    base = trimmed[: -len("/v1")] if trimmed.endswith("/v1") else trimmed
    return f"{base}/model/info"


def parse_model_info(body: str) -> dict[str, ResolvedModel]:
    """Parse a ``/model/info`` response body into an alias → entry dict, sorted by
    alias so diagnostics print the same order every run (Rust returns a
    ``BTreeMap``).

    Raises :class:`ValueError` when the body is not valid JSON or is missing the
    ``data`` array."""
    try:
        doc = json.loads(body)
    except json.JSONDecodeError as exc:
        raise ValueError(f"parsing /model/info response: {exc}") from exc
    if not isinstance(doc, dict) or not isinstance(doc.get("data"), list):
        raise ValueError("parsing /model/info response: missing `data` array")

    entries = {
        entry["model_name"]: ResolvedModel(
            alias=entry["model_name"],
            upstream=(entry.get("litellm_params") or {}).get("model"),
            id=(entry.get("model_info") or {}).get("id"),
        )
        for entry in doc["data"]
    }
    return {alias: entries[alias] for alias in sorted(entries)}


def _get_model_info(url: str, api_key: str, timeout: float) -> str:
    """Blocking GET, run off the event loop by :func:`fetch_model_info`.

    urllib rather than httpx: httpx is only a transitive dependency here (via the
    openai SDK), and one admin GET does not justify promoting it to a declared one.
    """
    request = urllib.request.Request(url, headers={"Authorization": f"Bearer {api_key}"})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:  # noqa: S310 - fixed https gateway URL
            return response.read().decode()
    except urllib.error.HTTPError as exc:
        raise ValueError(f"GET {url} returned {exc.code}: {exc.read().decode(errors='replace')}") from exc


async def fetch_model_info(api_url: str, api_key: str, timeout: float = 10.0) -> dict[str, ResolvedModel]:
    """Ask a LiteLLM gateway for its alias → upstream map.

    A 401 means the provider's API key is missing or rejected; either way the
    caller cannot see the mapping."""
    url = build_model_info_url(api_url)
    return parse_model_info(await asyncio.to_thread(_get_model_info, url, api_key, timeout))
