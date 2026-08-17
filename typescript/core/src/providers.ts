/**
 * Provider routing — the TypeScript port of the Rust reference engine's
 * `providers.rs`, `quirks.rs` and `resolution.rs`.
 *
 * Three concerns, one module because they are one story: **which** model a given
 * activity should use, **what** wire quirks that concrete model has, and — when
 * the route points at a LiteLLM-style gateway — **which** upstream model a
 * semantic alias actually resolves to.
 *
 * - {@link ProviderRegistry} holds provider credentials/URLs and a
 *   {@link ModelRouting} table mapping each {@link Activity} to a
 *   {@link ModelSlot}. {@link ProviderRegistry.llmConfigFor} walks the slot's
 *   fallback chain until it finds a registered provider.
 * - {@link quirksForModel} looks up per-model wire quirks by substring on the
 *   concrete upstream name.
 * - {@link buildModelInfoUrl} / {@link parseModelInfo} / {@link fetchModelInfo}
 *   recover the gateway's alias → upstream map from `GET /model/info`.
 *
 * The on-disk JSON shape is shared with the Rust CLI (`~/.smooth/providers.json`),
 * so the serialized keys are snake_case and must stay byte-compatible: the same
 * file is written by one engine and read by another. Legacy `thinking` /
 * `planning` field names still deserialize onto the merged `reasoning` slot.
 *
 * Routing values are pinned across all five engines by the shared corpus at
 * `spec/providers/routing.json` — a slot that resolves to the wrong model or
 * base URL sends real traffic and real money somewhere nobody intended, and it
 * looks like it is working.
 */

import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';

import type { ChatClientLike } from './agent.js';
import { createGatewayClient } from './gatewayClient.js';

// ---------------------------------------------------------------------------
// Providers
// ---------------------------------------------------------------------------

/**
 * The wire dialect a provider speaks. The string values match the Rust
 * reference's serde output so `providers.json` round-trips between engines.
 */
export enum ApiFormat {
    /** The OpenAI `/chat/completions` dialect. */
    OpenAiCompat = 'OpenAiCompat',
    /** Anthropic's native `/messages` dialect. */
    Anthropic = 'Anthropic',
}

/** Connection detail for a single LLM provider. */
export interface ProviderConfig {
    id: string;
    apiUrl: string;
    apiKey: string;
    apiFormat: ApiFormat;
    defaultModel: string;
}

/** OpenRouter — an OpenAI-compatible proxy for many models. */
export function openRouterProvider(apiKey: string): ProviderConfig {
    return { id: 'openrouter', apiUrl: 'https://openrouter.ai/api/v1', apiKey, apiFormat: ApiFormat.OpenAiCompat, defaultModel: 'openai/gpt-4o' };
}

/** The OpenAI direct API. */
export function openAiProvider(apiKey: string): ProviderConfig {
    return { id: 'openai', apiUrl: 'https://api.openai.com/v1', apiKey, apiFormat: ApiFormat.OpenAiCompat, defaultModel: 'gpt-4o' };
}

/** The Anthropic native API. */
export function anthropicProvider(apiKey: string): ProviderConfig {
    return { id: 'anthropic', apiUrl: 'https://api.anthropic.com/v1', apiKey, apiFormat: ApiFormat.Anthropic, defaultModel: 'claude-sonnet-4-20250514' };
}

/** A local Ollama instance — no API key needed. */
export function ollamaProvider(): ProviderConfig {
    return { id: 'ollama', apiUrl: 'http://localhost:11434/v1', apiKey: '', apiFormat: ApiFormat.OpenAiCompat, defaultModel: 'llama3' };
}

/** The Google Gemini API (OpenAI-compatible surface). */
export function googleProvider(apiKey: string): ProviderConfig {
    return {
        id: 'google',
        apiUrl: 'https://generativelanguage.googleapis.com/v1beta/openai',
        apiKey,
        apiFormat: ApiFormat.OpenAiCompat,
        defaultModel: 'gemini-2.0-flash',
    };
}

/** Moonshot AI's general-purpose API (OpenAI-compatible). */
export function kimiProvider(apiKey: string): ProviderConfig {
    return { id: 'kimi', apiUrl: 'https://api.moonshot.ai/v1', apiKey, apiFormat: ApiFormat.OpenAiCompat, defaultModel: 'kimi-k2.5' };
}

/** Moonshot's coding-optimized API (Anthropic-compatible). */
export function kimiCodeProvider(apiKey: string): ProviderConfig {
    return { id: 'kimi-code', apiUrl: 'https://api.kimi.com/coding/v1', apiKey, apiFormat: ApiFormat.Anthropic, defaultModel: 'kimi-for-coding' };
}

/** LLM Gateway — a unified API for 210+ models. */
export function llmGatewayProvider(apiKey: string): ProviderConfig {
    return { id: 'llmgateway', apiUrl: 'https://api.llmgateway.io/v1', apiKey, apiFormat: ApiFormat.OpenAiCompat, defaultModel: 'openai/gpt-4o' };
}

/**
 * The hosted LiteLLM-backed gateway run by Smoo AI.
 *
 * One API key, one URL, OpenAI-compatible. The gateway handles provider
 * selection, billing, moderation and cost tracking server-side, so consumers
 * reference models by semantic aliases (`smooth-coding`, `smooth-judge`, …) that
 * the gateway maps to whichever underlying model is currently best — upgrades
 * ship server-side with no client release.
 *
 * `SMOOAI_GATEWAY_URL` overrides the base URL. Only an ABSENT variable takes the
 * default: a set-but-empty override yields an empty base URL, matching Rust.
 */
export function smooaiGatewayProvider(apiKey: string): ProviderConfig {
    const override = process.env.SMOOAI_GATEWAY_URL;
    return {
        id: 'smooai-gateway',
        apiUrl: override === undefined ? 'https://llm.smoo.ai/v1' : override,
        apiKey,
        apiFormat: ApiFormat.OpenAiCompat,
        defaultModel: 'smooth-default',
    };
}

// ---------------------------------------------------------------------------
// Presets
// ---------------------------------------------------------------------------

/** A ready-made provider + routing configuration. */
export enum Preset {
    /** The hosted Smoo AI gateway — the recommended default. */
    SmoaiGateway = 'SmoaiGateway',
    /** Chinese frontier models via OpenRouter — the cheapest option. */
    OpenRouterLowCost = 'OpenRouterLowCost',
    /** Chinese frontier models via LLM Gateway. */
    LlmGatewayLowCost = 'LlmGatewayLowCost',
    /** OpenAI models. */
    OpenAI = 'OpenAI',
    /** Anthropic Claude models. */
    Anthropic = 'Anthropic',
}

/** One row of {@link ALL_PRESETS}: CLI name, display label, description. */
export interface PresetInfo {
    name: string;
    label: string;
    description: string;
}

/**
 * Every preset. The first entry is the recommended default — `th auth login`
 * shows them in this order.
 */
export const ALL_PRESETS: readonly PresetInfo[] = [
    {
        name: 'smooai-gateway',
        label: 'Smoo AI Gateway (recommended)',
        description: 'Hosted LiteLLM gateway run by Smoo AI — billing, moderation, governance, 100+ models. One key, one URL, no config.',
    },
    {
        name: 'openrouter-low-cost',
        label: 'OpenRouter Low Cost',
        description: 'GLM-5.1 thinking (#1 SWE-Bench Pro), MiniMax-M2.7 coding (56% SWE-Pro, 10B params), DeepSeek-V3.2 default',
    },
    {
        name: 'llmgateway-low-cost',
        label: 'LLM Gateway Low Cost',
        description: 'GLM-5 thinking, MiniMax-M2.7 coding, DeepSeek-V3.2 default — unified billing, 224 models',
    },
    { name: 'openai', label: 'OpenAI', description: 'o3-mini thinking, GPT-4o coding — OpenAI ecosystem' },
    { name: 'anthropic', label: 'Anthropic', description: 'Claude Opus thinking, Sonnet coding — highest quality' },
];

/** Parse a preset name or alias. Returns `undefined` for unknown names. */
export function presetFromName(name: string): Preset | undefined {
    switch (name) {
        case 'smooai-gateway':
        case 'smooai':
        case 'gateway':
            return Preset.SmoaiGateway;
        case 'openrouter-low-cost':
        case 'low-cost':
            return Preset.OpenRouterLowCost;
        case 'llmgateway-low-cost':
        case 'gateway-low-cost':
            return Preset.LlmGatewayLowCost;
        case 'openai':
        case 'codex':
            return Preset.OpenAI;
        case 'anthropic':
            return Preset.Anthropic;
        default:
            return undefined;
    }
}

/** The provider id a preset requires. */
export function presetProviderId(preset: Preset): string {
    switch (preset) {
        case Preset.SmoaiGateway:
            return 'smooai-gateway';
        case Preset.OpenRouterLowCost:
            return 'openrouter';
        case Preset.LlmGatewayLowCost:
            return 'llmgateway';
        case Preset.OpenAI:
            return 'openai';
        case Preset.Anthropic:
            return 'anthropic';
    }
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

/**
 * Selects which model slot a call routes through. Six semantic slots: the legacy
 * `Thinking` + `Planning` split collapsed into {@link Activity.Reasoning}, and the
 * legacy "default" alias is served by {@link Activity.Coding}.
 */
export enum Activity {
    /** The outer coding loop — the workhorse slot, which also serves the legacy "default" call path. */
    Coding = 'Coding',
    /** Deep reasoning / planning / chain-of-thought. */
    Reasoning = 'Reasoning',
    /** Code review, critique, adversarial checks. */
    Reviewing = 'Reviewing',
    /** LLM-as-a-judge: yes/no verdicts, low latency, used by Narc guardrails and bench scoring. */
    Judge = 'Judge',
    /** Context compression during long agent runs. */
    Summarize = 'Summarize',
    /**
     * Small, latency-sensitive utility calls: session auto-naming, short titles,
     * one-liner summaries, autocomplete. Sub-second first token, short output, no
     * tool use — don't pay Sonnet-plus prices to name a session.
     */
    Fast = 'Fast',
}

/** A provider id + model name, with an optional fallback used when the provider is not registered. */
export interface ModelSlot {
    provider: string;
    model: string;
    fallback?: ModelSlot;
}

/** Build a slot with no fallback. */
export function modelSlot(provider: string, model: string): ModelSlot {
    return { provider, model };
}

/** Return a copy of `slot` with `fallback` attached. */
export function withFallback(slot: ModelSlot, fallback: ModelSlot): ModelSlot {
    return { ...slot, fallback };
}

/**
 * The per-activity routing table.
 *
 * Six semantic slots plus a `default` slot kept for wire compatibility: no
 * {@link Activity} routes through `default` directly ({@link Activity.Coding}
 * serves the default path), but the field stays so pre-collapse configs load.
 */
export interface ModelRouting {
    coding: ModelSlot;
    /** Merged deep-reasoning slot. Absent in older files (which carry `thinking`); falls back to `default`. */
    reasoning?: ModelSlot;
    reviewing: ModelSlot;
    judge: ModelSlot;
    summarize: ModelSlot;
    default: ModelSlot;
    /** Optional on disk: pre-`fast` files fall back to `default` at lookup time. */
    fast?: ModelSlot;
    /** Legacy field, deserialized but ignored at lookup time — `reasoning` absorbed it. */
    planning?: ModelSlot;
}

/**
 * The neutral, provider-agnostic routing every slot starts on: the well-known
 * `openrouter` provider id with a placeholder `auto` model, so the library ships
 * no opinion about a specific hosted gateway. Consumers opt into the Smoo AI
 * gateway via {@link Preset.SmoaiGateway} explicitly.
 */
export function defaultModelRouting(): ModelRouting {
    const slot = (): ModelSlot => modelSlot('openrouter', 'openrouter/auto');
    return { coding: slot(), reasoning: slot(), reviewing: slot(), judge: slot(), summarize: slot(), default: slot(), fast: slot() };
}

/** The slot for an activity. `Reasoning` and `Fast` fall back to `default` when absent. */
export function slotFor(routing: ModelRouting, activity: Activity): ModelSlot {
    switch (activity) {
        case Activity.Coding:
            return routing.coding;
        case Activity.Reasoning:
            return routing.reasoning ?? routing.default;
        case Activity.Reviewing:
            return routing.reviewing;
        case Activity.Judge:
            return routing.judge;
        case Activity.Summarize:
            return routing.summarize;
        case Activity.Fast:
            return routing.fast ?? routing.default;
    }
}

function uniformRouting(slot: ModelSlot): ModelRouting {
    return { coding: slot, reasoning: slot, reviewing: slot, judge: slot, summarize: slot, default: slot, fast: slot };
}

// ---------------------------------------------------------------------------
// Resolved config
// ---------------------------------------------------------------------------

/**
 * A fully resolved route: the provider connection plus the model the activity
 * picked. Feed `apiUrl`/`apiKey` to {@link createGatewayClient}.
 */
export interface LlmConfig {
    apiUrl: string;
    apiKey: string;
    model: string;
    maxTokens: number;
    temperature: number;
    apiFormat: ApiFormat;
}

// ---------------------------------------------------------------------------
// On-disk shape
// ---------------------------------------------------------------------------

/** The on-disk JSON for one provider — snake_case, shared with the Rust CLI. */
interface ProviderWire {
    id: string;
    api_url: string;
    api_key: string;
    api_format: ApiFormat;
    default_model: string;
}

interface SlotWire {
    provider: string;
    model: string;
    fallback?: SlotWire;
}

interface RoutingWire {
    coding: SlotWire;
    reasoning?: SlotWire;
    /** Legacy name for `reasoning`; migrated on read, never written. */
    thinking?: SlotWire;
    reviewing: SlotWire;
    judge: SlotWire;
    summarize: SlotWire;
    default: SlotWire;
    fast?: SlotWire;
    planning?: SlotWire;
}

interface RegistryWire {
    providers: ProviderWire[];
    routing: RoutingWire;
}

const slotFromWire = (w: SlotWire): ModelSlot => ({
    provider: w.provider,
    model: w.model,
    ...(w.fallback ? { fallback: slotFromWire(w.fallback) } : {}),
});

const slotToWire = (s: ModelSlot): SlotWire => ({
    provider: s.provider,
    model: s.model,
    ...(s.fallback ? { fallback: slotToWire(s.fallback) } : {}),
});

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

/** Registered providers plus the per-activity routing table. */
export class ProviderRegistry {
    private readonly providers = new Map<string, ProviderConfig>();

    /** The per-activity table. Reassign a slot to re-point a route. */
    routing: ModelRouting = defaultModelRouting();

    /**
     * A registry pre-configured with a preset: registers the preset's provider
     * and installs routing tuned for the preset's goals (cost, quality, latency).
     */
    static fromPreset(preset: Preset, apiKey: string): ProviderRegistry {
        const registry = new ProviderRegistry();
        switch (preset) {
            case Preset.SmoaiGateway:
                // Semantic aliases the gateway's LiteLLM config maps to whichever
                // underlying model is currently best. Changing the underlying model
                // is a server-side deploy — no client release needed.
                registry.registerProvider(smooaiGatewayProvider(apiKey));
                registry.routing = {
                    coding: modelSlot('smooai-gateway', 'smooth-coding'),
                    reasoning: modelSlot('smooai-gateway', 'smooth-reasoning'),
                    reviewing: modelSlot('smooai-gateway', 'smooth-reviewing'),
                    judge: modelSlot('smooai-gateway', 'smooth-judge'),
                    summarize: modelSlot('smooai-gateway', 'smooth-summarize'),
                    default: modelSlot('smooai-gateway', 'smooth-default'),
                    fast: modelSlot('smooai-gateway', 'smooth-fast'),
                };
                break;
            case Preset.OpenRouterLowCost:
                // OpenRouter uses provider-prefixed model IDs.
                registry.registerProvider(openRouterProvider(apiKey));
                registry.routing = {
                    coding: withFallback(modelSlot('openrouter', 'minimax/minimax-m2.7'), modelSlot('openrouter', 'minimax/minimax-m2.5')),
                    reasoning: modelSlot('openrouter', 'z-ai/glm-5.1'),
                    reviewing: modelSlot('openrouter', 'deepseek/deepseek-v3.2'),
                    judge: modelSlot('openrouter', 'google/gemini-2.5-flash'),
                    summarize: modelSlot('openrouter', 'deepseek/deepseek-v3.2'),
                    default: modelSlot('openrouter', 'deepseek/deepseek-v3.2'),
                    fast: modelSlot('openrouter', 'google/gemini-2.5-flash-lite'),
                };
                break;
            case Preset.LlmGatewayLowCost:
                // LLM Gateway uses bare model names.
                registry.registerProvider(llmGatewayProvider(apiKey));
                registry.routing = {
                    coding: withFallback(modelSlot('llmgateway', 'minimax-m2.7'), modelSlot('llmgateway', 'minimax-m2.5')),
                    reasoning: modelSlot('llmgateway', 'glm-5'),
                    reviewing: modelSlot('llmgateway', 'deepseek-v3.2'),
                    judge: modelSlot('llmgateway', 'gemini-2.5-flash'),
                    summarize: modelSlot('llmgateway', 'deepseek-v3.2'),
                    default: modelSlot('llmgateway', 'deepseek-v3.2'),
                    fast: modelSlot('llmgateway', 'gemini-2.5-flash-lite'),
                };
                break;
            case Preset.OpenAI:
                registry.registerProvider(openAiProvider(apiKey));
                registry.routing = {
                    coding: modelSlot('openai', 'gpt-4o'),
                    reasoning: modelSlot('openai', 'o3-mini'),
                    reviewing: modelSlot('openai', 'gpt-4o'),
                    judge: modelSlot('openai', 'gpt-4o-mini'),
                    summarize: modelSlot('openai', 'gpt-4o-mini'),
                    default: modelSlot('openai', 'gpt-4o'),
                    fast: modelSlot('openai', 'gpt-4o-mini'),
                };
                break;
            case Preset.Anthropic:
                registry.registerProvider(anthropicProvider(apiKey));
                registry.routing = {
                    coding: modelSlot('anthropic', 'claude-sonnet-4-20250514'),
                    reasoning: modelSlot('anthropic', 'claude-opus-4-20250514'),
                    reviewing: modelSlot('anthropic', 'claude-sonnet-4-20250514'),
                    judge: modelSlot('anthropic', 'claude-haiku-4-5-20251001'),
                    summarize: modelSlot('anthropic', 'claude-haiku-4-5-20251001'),
                    default: modelSlot('anthropic', 'claude-sonnet-4-20250514'),
                    fast: modelSlot('anthropic', 'claude-haiku-4-5-20251001'),
                };
                break;
        }
        return registry;
    }

    /**
     * A minimal registry from `SMOOTH_API_KEY` (required), `SMOOTH_PROVIDER`
     * (defaults to `openrouter`) and `SMOOTH_MODEL` (optional). Returns
     * `undefined` when `SMOOTH_API_KEY` is unset — never a keyless client.
     */
    static fromEnv(): ProviderRegistry | undefined {
        const apiKey = process.env.SMOOTH_API_KEY;
        if (apiKey === undefined) return undefined;
        const providerId = process.env.SMOOTH_PROVIDER || 'openrouter';

        let config: ProviderConfig;
        switch (providerId) {
            case 'openai':
                config = openAiProvider(apiKey);
                break;
            case 'anthropic':
                config = anthropicProvider(apiKey);
                break;
            case 'ollama':
                config = { ...ollamaProvider(), apiKey };
                break;
            case 'google':
                config = googleProvider(apiKey);
                break;
            case 'kimi':
                config = kimiProvider(apiKey);
                break;
            case 'kimi-code':
                config = kimiCodeProvider(apiKey);
                break;
            case 'llmgateway':
                config = llmGatewayProvider(apiKey);
                break;
            default:
                config = openRouterProvider(apiKey);
        }

        const registry = new ProviderRegistry();
        registry.registerProvider(config);
        registry.routing = uniformRouting(modelSlot(providerId, process.env.SMOOTH_MODEL || config.defaultModel));
        return registry;
    }

    /** Read a registry from a JSON file (e.g. `~/.smooth/providers.json`). */
    static loadFromFile(path: string): ProviderRegistry {
        return ProviderRegistry.fromJson(readFileSync(path, 'utf8'));
    }

    /** Deserialize a registry from the JSON shape {@link ProviderRegistry.toJson} writes. */
    static fromJson(json: string): ProviderRegistry {
        const file = JSON.parse(json) as RegistryWire;
        const registry = new ProviderRegistry();
        const w = file.routing;
        registry.routing = {
            coding: slotFromWire(w.coding),
            // An explicit `reasoning` always wins over the legacy `thinking`.
            ...(w.reasoning ?? w.thinking ? { reasoning: slotFromWire((w.reasoning ?? w.thinking)!) } : {}),
            reviewing: slotFromWire(w.reviewing),
            judge: slotFromWire(w.judge),
            summarize: slotFromWire(w.summarize),
            default: slotFromWire(w.default),
            ...(w.fast ? { fast: slotFromWire(w.fast) } : {}),
            ...(w.planning ? { planning: slotFromWire(w.planning) } : {}),
        };
        for (const p of file.providers ?? []) {
            registry.registerProvider({ id: p.id, apiUrl: p.api_url, apiKey: p.api_key, apiFormat: p.api_format, defaultModel: p.default_model });
        }
        return registry;
    }

    /** Add (or replace) a provider configuration. */
    registerProvider(config: ProviderConfig): void {
        this.providers.set(config.id, config);
    }

    /** Drop a provider by id. */
    removeProvider(id: string): void {
        this.providers.delete(id);
    }

    /** Look up a provider by id. */
    getProvider(id: string): ProviderConfig | undefined {
        return this.providers.get(id);
    }

    /** Every registered provider id, sorted. */
    listProviders(): string[] {
        return [...this.providers.keys()].sort();
    }

    /** Point every routing slot at `providerId` using its default model. */
    setDefaultProvider(providerId: string): void {
        this.routing = uniformRouting(modelSlot(providerId, this.providers.get(providerId)?.defaultModel ?? ''));
    }

    /** Install a custom routing table. */
    withRouting(routing: ModelRouting): this {
        this.routing = routing;
        return this;
    }

    private resolveSlot(slot: ModelSlot): LlmConfig {
        const provider = this.providers.get(slot.provider);
        if (provider) {
            return {
                apiUrl: provider.apiUrl,
                apiKey: provider.apiKey,
                model: slot.model,
                maxTokens: 32768,
                temperature: 0.0,
                apiFormat: provider.apiFormat,
            };
        }
        if (slot.fallback) return this.resolveSlot(slot.fallback);
        throw new Error(`provider '${slot.provider}' not registered and no fallback available`);
    }

    /**
     * Resolve the route for an activity. Throws when the slot's provider — and
     * every fallback — is unregistered, rather than silently substituting some
     * other provider.
     */
    llmConfigFor(activity: Activity): LlmConfig {
        return this.resolveSlot(slotFor(this.routing, activity));
    }

    /** Resolve the wire-compat `default` slot. */
    defaultLlmConfig(): LlmConfig {
        return this.resolveSlot(this.routing.default);
    }

    /**
     * Build a gateway client for an activity's resolved route — the one line
     * between "which model should this call use" and a client that speaks to it.
     *
     * The client is OpenAI-compatible; an {@link ApiFormat.Anthropic} provider is
     * rejected rather than silently spoken to in the wrong dialect.
     */
    clientFor(activity: Activity): { client: ChatClientLike; config: LlmConfig } {
        const config = this.llmConfigFor(activity);
        if (config.apiFormat !== ApiFormat.OpenAiCompat) {
            throw new Error(`activity ${activity} routes to a ${config.apiFormat} provider, which the OpenAI-compatible gateway client cannot speak`);
        }
        return { client: createGatewayClient({ baseURL: config.apiUrl, apiKey: config.apiKey }), config };
    }

    /** Serialize to the on-disk JSON shape, snake_case keys and all. */
    toJson(pretty = false): string {
        const providers: ProviderWire[] = this.listProviders().map((id) => {
            const p = this.providers.get(id)!;
            return { id: p.id, api_url: p.apiUrl, api_key: p.apiKey, api_format: p.apiFormat, default_model: p.defaultModel };
        });
        const r = this.routing;
        const routing: RoutingWire = {
            coding: slotToWire(r.coding),
            ...(r.reasoning ? { reasoning: slotToWire(r.reasoning) } : {}),
            reviewing: slotToWire(r.reviewing),
            judge: slotToWire(r.judge),
            summarize: slotToWire(r.summarize),
            default: slotToWire(r.default),
            ...(r.fast ? { fast: slotToWire(r.fast) } : {}),
            ...(r.planning ? { planning: slotToWire(r.planning) } : {}),
        };
        const file: RegistryWire = { providers, routing };
        return pretty ? JSON.stringify(file, null, 2) : JSON.stringify(file);
    }

    /** Write the registry as pretty-printed JSON, creating parent directories. */
    saveToFile(path: string): void {
        mkdirSync(dirname(path), { recursive: true });
        writeFileSync(path, this.toJson(true));
    }
}

// ---------------------------------------------------------------------------
// Per-model wire quirks
// ---------------------------------------------------------------------------

/**
 * Per-model wire-format flags. Populate a field only when the quirk is worth the
 * branch — every conditional is a place for drift.
 *
 * When routing through a LiteLLM-style gateway the concrete upstream model only
 * reveals itself in response headers (`x-litellm-model-name`), by which point the
 * request is already sent. So prefer always-safe request shapes over per-model
 * conditionals, and keep this table for the cases where the strict form does not
 * work everywhere.
 */
export interface ModelQuirks {
    /** When `false`, force `parallel_tool_calls` off even if the agent config requests it. */
    allowParallelTools?: boolean;
    /** Ask the client to be extra careful about tool_call echo shape. Nothing reads this yet. */
    strictToolCallJson: boolean;
}

const QUIRKS_TABLE: readonly (readonly [string, ModelQuirks])[] = [
    ['qwen3-coder', { strictToolCallJson: true }],
    ['qwen-coder', { strictToolCallJson: true }],
];

/**
 * Look up quirks by concrete upstream name. Matching is case-insensitive and
 * substring-based, so minor version drift (`qwen3-coder-plus-2025-04`) still hits
 * the `qwen3-coder` entry. Returns safe defaults when nothing matches.
 */
export function quirksForModel(upstream: string): ModelQuirks {
    const lc = upstream.toLowerCase();
    for (const [needle, quirks] of QUIRKS_TABLE) {
        if (lc.includes(needle)) return { ...quirks };
    }
    return { strictToolCallJson: false };
}

/** The quirk table's canonical keys, for diagnostics. */
export function quirkKeys(): string[] {
    return QUIRKS_TABLE.map(([needle]) => needle);
}

/** Every quirk entry matching an upstream name. Usually one wins; the full set is kept for tests. */
export function quirksDebugSnapshot(upstream: string): Record<string, ModelQuirks> {
    const lc = upstream.toLowerCase();
    const out: Record<string, ModelQuirks> = {};
    for (const [needle, quirks] of QUIRKS_TABLE) {
        if (lc.includes(needle)) out[needle] = { ...quirks };
    }
    return out;
}

// ---------------------------------------------------------------------------
// LiteLLM alias resolution
// ---------------------------------------------------------------------------

/** One routing entry returned by a gateway's `/model/info`. */
export interface ResolvedModel {
    /** The name callers use (e.g. `smooth-coding`). */
    alias: string;
    /** The concrete model (e.g. `moonshot/kimi-k2-thinking`), when the gateway surfaces it. */
    upstream?: string;
    /** Stable id from `model_info.id`, useful for tracing a rename. */
    id?: string;
}

/**
 * Derive the `/model/info` URL from a provider's OpenAI-compat `apiUrl`
 * (e.g. `https://llm.smoo.ai/v1`). Stripping `/v1` is safe: `/model/info` lives
 * at the gateway root in every LiteLLM deployment seen.
 */
export function buildModelInfoUrl(apiUrl: string): string {
    const trimmed = apiUrl.replace(/\/+$/, '');
    const base = trimmed.endsWith('/v1') ? trimmed.slice(0, -'/v1'.length) : trimmed;
    return `${base}/model/info`;
}

interface ModelInfoWire {
    data?: Array<{ model_name: string; litellm_params?: { model?: string }; model_info?: { id?: string } }>;
}

/**
 * Parse a `/model/info` response body into an alias → entry map, sorted by alias
 * so diagnostics print the same order every run (Rust returns a `BTreeMap`).
 *
 * Throws when the body is not valid JSON or is missing the `data` array.
 */
export function parseModelInfo(body: string): Map<string, ResolvedModel> {
    let doc: ModelInfoWire;
    try {
        doc = JSON.parse(body) as ModelInfoWire;
    } catch (error) {
        throw new Error(`parsing /model/info response: ${error instanceof Error ? error.message : String(error)}`);
    }
    if (!Array.isArray(doc?.data)) {
        throw new Error('parsing /model/info response: missing `data` array');
    }
    const entries = doc.data.map((entry): [string, ResolvedModel] => [
        entry.model_name,
        {
            alias: entry.model_name,
            ...(entry.litellm_params?.model !== undefined ? { upstream: entry.litellm_params.model } : {}),
            ...(entry.model_info?.id !== undefined ? { id: entry.model_info.id } : {}),
        },
    ]);
    entries.sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
    return new Map(entries);
}

/**
 * Ask a LiteLLM gateway for its alias → upstream map.
 *
 * A 401 means the provider's API key is missing or rejected; either way the
 * caller cannot see the mapping.
 */
export async function fetchModelInfo(apiUrl: string, apiKey: string, timeoutMs = 10_000): Promise<Map<string, ResolvedModel>> {
    const url = buildModelInfoUrl(apiUrl);
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    try {
        const response = await fetch(url, { headers: { Authorization: `Bearer ${apiKey}` }, signal: controller.signal });
        const body = await response.text();
        if (!response.ok) {
            throw new Error(`GET ${url} returned ${response.status}: ${body}`);
        }
        return parseModelInfo(body);
    } finally {
        clearTimeout(timer);
    }
}
