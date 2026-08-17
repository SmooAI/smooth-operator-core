/**
 * Provider-routing parity tests — the TypeScript half of the cross-language contract.
 *
 * The first block is the drift gate: it replays `spec/providers/routing.json`
 * (generated FROM the Rust reference) and asserts this port resolves every preset
 * slot to the same model, base URL, key and wire format, matches the same quirks,
 * builds the same `/model/info` URLs, and parses the same alias maps. The rest
 * port the Rust engine's own unit tests — fallback chains, on-disk wire
 * compatibility, env loading, save/load round-trip.
 */

import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
    Activity,
    ApiFormat,
    ALL_PRESETS,
    anthropicProvider,
    buildModelInfoUrl,
    defaultModelRouting,
    googleProvider,
    kimiCodeProvider,
    kimiProvider,
    llmGatewayProvider,
    modelSlot,
    ollamaProvider,
    openAiProvider,
    openRouterProvider,
    parseModelInfo,
    Preset,
    presetFromName,
    presetProviderId,
    ProviderRegistry,
    quirksDebugSnapshot,
    quirksForModel,
    slotFor,
    smooaiGatewayProvider,
    withFallback,
} from '../src/providers.js';
import type { ProviderConfig } from '../src/providers.js';

interface CorpusSlot {
    model: string;
    apiUrl: string;
    apiKey: string;
    apiFormat: string;
    maxTokens: number;
    temperature: number;
}

interface Corpus {
    presetNames: Array<{ name: string; preset: string | null }>;
    defaultRouting: Record<string, { provider: string; model: string }>;
    wireCompat: Array<{ id: string; json: string; slotModels: Record<string, string> }>;
    fallbackChain: { apiUrl: string; model: string; apiKey: string };
    unregisteredWithoutFallbackErrors: boolean;
    presets: Array<{ name: string; providerId: string; registeredProviders: string[]; slots: Record<string, CorpusSlot> }>;
    providerFactories: Array<{ factory: string; id: string; apiUrl: string; apiKey: string; apiFormat: string; defaultModel: string }>;
    quirks: Array<{ upstream: string; strictToolCallJson: boolean; allowParallelTools: boolean | null; matchedKeys: string[] }>;
    modelInfoUrls: Array<{ apiUrl: string; modelInfoUrl: string }>;
    modelInfoParse: Array<{ id: string; body: string; entries: Array<{ alias: string; upstream: string | null; id: string | null }> }>;
}

const CORPUS_PATH = join(dirname(fileURLToPath(import.meta.url)), '..', '..', '..', 'spec', 'providers', 'routing.json');
const CORPUS: Corpus = JSON.parse(readFileSync(CORPUS_PATH, 'utf8'));

const ACTIVITIES: Record<string, Activity> = {
    coding: Activity.Coding,
    reasoning: Activity.Reasoning,
    reviewing: Activity.Reviewing,
    judge: Activity.Judge,
    summarize: Activity.Summarize,
    fast: Activity.Fast,
};

const resolve = (registry: ProviderRegistry, label: string) => (label === 'default' ? registry.defaultLlmConfig() : registry.llmConfigFor(ACTIVITIES[label]!));

// The corpus pins the production gateway URL, which only applies when
// SMOOAI_GATEWAY_URL is ABSENT.
let priorGatewayUrl: string | undefined;
beforeEach(() => {
    priorGatewayUrl = process.env.SMOOAI_GATEWAY_URL;
    delete process.env.SMOOAI_GATEWAY_URL;
});
afterEach(() => {
    if (priorGatewayUrl === undefined) delete process.env.SMOOAI_GATEWAY_URL;
    else process.env.SMOOAI_GATEWAY_URL = priorGatewayUrl;
});

describe('provider routing — shared corpus (spec/providers/routing.json)', () => {
    it('carries all five presets', () => {
        expect(CORPUS.presets).toHaveLength(5);
    });

    for (const preset of CORPUS.presets) {
        describe(preset.name, () => {
            it('registers the preset provider', () => {
                const parsed = presetFromName(preset.name);
                expect(parsed).toBeDefined();
                expect(presetProviderId(parsed!)).toBe(preset.providerId);
                expect(ProviderRegistry.fromPreset(parsed!, 'test-key').listProviders()).toEqual(preset.registeredProviders);
            });

            for (const [label, want] of Object.entries(preset.slots)) {
                it(`routes ${label} to ${want.model}`, () => {
                    const registry = ProviderRegistry.fromPreset(presetFromName(preset.name)!, 'test-key');
                    expect(resolve(registry, label)).toEqual({
                        apiUrl: want.apiUrl,
                        apiKey: want.apiKey,
                        model: want.model,
                        maxTokens: want.maxTokens,
                        temperature: want.temperature,
                        apiFormat: want.apiFormat as ApiFormat,
                    });
                });
            }
        });
    }

    it('parses every preset name and alias', () => {
        for (const v of CORPUS.presetNames) {
            const parsed = presetFromName(v.name);
            if (v.preset === null) expect(parsed, `${v.name} must not resolve`).toBeUndefined();
            else expect(presetProviderId(parsed!), v.name).toBe(v.preset);
        }
    });

    it('builds every provider factory identically', () => {
        const factories: Record<string, ProviderConfig> = {
            openrouter: openRouterProvider('k'),
            openai: openAiProvider('k'),
            anthropic: anthropicProvider('k'),
            ollama: ollamaProvider(),
            google: googleProvider('k'),
            kimi: kimiProvider('k'),
            kimiCode: kimiCodeProvider('k'),
            llmgateway: llmGatewayProvider('k'),
            smooaiGateway: smooaiGatewayProvider('k'),
        };
        for (const want of CORPUS.providerFactories) {
            expect(factories[want.factory], want.factory).toEqual({
                id: want.id,
                apiUrl: want.apiUrl,
                apiKey: want.apiKey,
                apiFormat: want.apiFormat as ApiFormat,
                defaultModel: want.defaultModel,
            });
        }
    });

    it('ships the neutral, provider-agnostic default routing', () => {
        const routing = defaultModelRouting();
        for (const [label, want] of Object.entries(CORPUS.defaultRouting)) {
            const slot = label === 'default' ? routing.default : slotFor(routing, ACTIVITIES[label]!);
            expect({ provider: slot.provider, model: slot.model }, label).toEqual(want);
        }
        // The hosted gateway is opt-in, never the default.
        expect(routing.coding.provider).not.toBe('smooai-gateway');
    });

    it('keeps on-disk wire compatibility with legacy slot names', () => {
        for (const v of CORPUS.wireCompat) {
            const registry = ProviderRegistry.fromJson(v.json);
            for (const [label, want] of Object.entries(v.slotModels)) {
                const slot = label === 'default' ? registry.routing.default : slotFor(registry.routing, ACTIVITIES[label]!);
                expect(slot.model, `${v.id}/${label}`).toBe(want);
            }
        }
    });

    it('resolves through a fallback chain to the registered provider', () => {
        const registry = new ProviderRegistry();
        registry.registerProvider({ id: 'tertiary', apiUrl: 'https://tertiary.example.com/v1', apiKey: 't-key', apiFormat: ApiFormat.OpenAiCompat, defaultModel: 'model-c' });
        registry.routing.coding = withFallback(modelSlot('primary', 'model-a'), withFallback(modelSlot('secondary', 'model-b'), modelSlot('tertiary', 'model-c')));

        const config = registry.llmConfigFor(Activity.Coding);
        expect({ apiUrl: config.apiUrl, model: config.model, apiKey: config.apiKey }).toEqual(CORPUS.fallbackChain);
    });

    it('throws when a slot is unregistered and has no fallback', () => {
        const registry = new ProviderRegistry();
        registry.routing.coding = modelSlot('nope', 'm');
        expect(CORPUS.unregisteredWithoutFallbackErrors).toBe(true);
        expect(() => registry.llmConfigFor(Activity.Coding)).toThrow(/not registered/);
    });

    it('matches the same per-model quirks', () => {
        for (const v of CORPUS.quirks) {
            const quirks = quirksForModel(v.upstream);
            expect(quirks.strictToolCallJson, v.upstream).toBe(v.strictToolCallJson);
            expect(quirks.allowParallelTools ?? null, v.upstream).toBe(v.allowParallelTools);
            expect(Object.keys(quirksDebugSnapshot(v.upstream)).sort(), v.upstream).toEqual(v.matchedKeys);
        }
    });

    it('builds the same /model/info URLs', () => {
        for (const v of CORPUS.modelInfoUrls) {
            expect(buildModelInfoUrl(v.apiUrl), v.apiUrl).toBe(v.modelInfoUrl);
        }
    });

    it('parses the same alias maps, alias-sorted', () => {
        for (const v of CORPUS.modelInfoParse) {
            const parsed = parseModelInfo(v.body);
            expect([...parsed.keys()], v.id).toEqual(v.entries.map((e) => e.alias));
            for (const want of v.entries) {
                const entry = parsed.get(want.alias)!;
                expect(entry.upstream ?? null, `${v.id}/${want.alias}`).toBe(want.upstream);
                expect(entry.id ?? null, `${v.id}/${want.alias}`).toBe(want.id);
            }
        }
    });
});

describe('provider routing — registry behaviour', () => {
    it('rejects an invalid /model/info body instead of returning an empty map', () => {
        expect(() => parseModelInfo('not json')).toThrow(/\/model\/info/);
        expect(() => parseModelInfo('{"nope":1}')).toThrow(/data/);
    });

    it('writes the on-disk shape the Rust CLI reads, and reads it back', () => {
        const dir = mkdtempSync(join(tmpdir(), 'routing-'));
        const path = join(dir, 'nested', 'providers.json');

        const registry = new ProviderRegistry();
        registry.registerProvider(openRouterProvider('or-key'));
        registry.registerProvider(openAiProvider('oai-key'));
        registry.saveToFile(path);

        const written = JSON.parse(readFileSync(path, 'utf8'));
        expect(Object.keys(written.providers[0]).sort()).toEqual(['api_format', 'api_key', 'api_url', 'default_model', 'id']);
        expect(Object.keys(written.routing).sort()).toEqual(['coding', 'default', 'fast', 'judge', 'reasoning', 'reviewing', 'summarize']);
        // `planning` is legacy: accepted on read, never written by a fresh config.
        expect(written.routing.planning).toBeUndefined();
        // A slot with no fallback omits the key entirely — `"fallback": null` is a different document.
        expect(Object.keys(written.routing.coding).sort()).toEqual(['model', 'provider']);

        const loaded = ProviderRegistry.loadFromFile(path);
        expect(loaded.listProviders()).toEqual(['openai', 'openrouter']);
        expect(loaded.getProvider('openrouter')?.apiKey).toBe('or-key');
        const config = loaded.llmConfigFor(Activity.Reasoning);
        expect(config.model).toBe('openrouter/auto');
        expect(config.apiKey).toBe('or-key');
    });

    it('round-trips a fallback chain through JSON', () => {
        const registry = ProviderRegistry.fromPreset(Preset.OpenRouterLowCost, 'k');
        const restored = ProviderRegistry.fromJson(registry.toJson());
        expect(restored.routing.coding.fallback?.model).toBe('minimax/minimax-m2.5');
        expect(restored.llmConfigFor(Activity.Coding).model).toBe('minimax/minimax-m2.7');
    });

    it('reads a minimal registry from the environment', () => {
        process.env.SMOOTH_API_KEY = 'env-test-key';
        process.env.SMOOTH_PROVIDER = 'openai';
        delete process.env.SMOOTH_MODEL;
        try {
            const registry = ProviderRegistry.fromEnv()!;
            expect(registry.getProvider('openai')?.apiKey).toBe('env-test-key');
            expect(registry.defaultLlmConfig().model).toBe('gpt-4o');

            process.env.SMOOTH_MODEL = 'gpt-4o-mini';
            expect(ProviderRegistry.fromEnv()!.defaultLlmConfig().model).toBe('gpt-4o-mini');
        } finally {
            delete process.env.SMOOTH_API_KEY;
            delete process.env.SMOOTH_PROVIDER;
            delete process.env.SMOOTH_MODEL;
        }
    });

    it('refuses to build a registry without an API key', () => {
        delete process.env.SMOOTH_API_KEY;
        expect(ProviderRegistry.fromEnv()).toBeUndefined();
    });

    it('honours the SMOOAI_GATEWAY_URL override', () => {
        process.env.SMOOAI_GATEWAY_URL = 'https://llm.dev.smooai.com/v1';
        const config = ProviderRegistry.fromPreset(Preset.SmoaiGateway, 'dev-key').defaultLlmConfig();
        expect(config.apiUrl).toBe('https://llm.dev.smooai.com/v1');
        expect(config.apiKey).toBe('dev-key');
    });

    it('points every slot at one provider via setDefaultProvider, and breaks when it is removed', () => {
        const registry = new ProviderRegistry();
        registry.registerProvider(kimiProvider('k-key'));
        registry.setDefaultProvider('kimi');
        for (const activity of Object.values(ACTIVITIES)) {
            const config = registry.llmConfigFor(activity);
            expect(config.model).toBe('kimi-k2.5');
            expect(config.apiUrl).toBe('https://api.moonshot.ai/v1');
        }
        registry.removeProvider('kimi');
        expect(() => registry.llmConfigFor(Activity.Coding)).toThrow();
    });

    it('lists the recommended preset first', () => {
        expect(ALL_PRESETS[0]!.name).toBe('smooai-gateway');
        expect(ALL_PRESETS[0]!.label).toContain('recommended');
        expect(ALL_PRESETS).toHaveLength(5);
    });

    // The integration point: a resolved route becomes a live client. An
    // Anthropic-dialect provider must be refused, not spoken to in OpenAI's format.
    it('builds a client for an OpenAI-compatible route and refuses an Anthropic one', () => {
        const { client, config } = ProviderRegistry.fromPreset(Preset.OpenAI, 'k').clientFor(Activity.Coding);
        expect(client.chat.completions.create).toBeTypeOf('function');
        expect(config.model).toBe('gpt-4o');

        expect(() => ProviderRegistry.fromPreset(Preset.Anthropic, 'k').clientFor(Activity.Coding)).toThrow(/cannot speak/);
    });
});
