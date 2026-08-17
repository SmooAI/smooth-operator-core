using System.Text.Json;
using System.Text.Json.Serialization;
using SmooAI.SmoothOperator.Core;

namespace SmooAI.SmoothOperator.Core.Tests;

/// <summary>
/// Provider-routing parity tests — the .NET half of the cross-language contract.
///
/// <para>The corpus tests are the drift gate: they replay <c>spec/providers/routing.json</c>
/// (generated FROM the Rust reference) and assert this port resolves every preset slot to the same
/// model, base URL, key and wire format, matches the same quirks, builds the same
/// <c>/model/info</c> URLs, and parses the same alias maps. The rest port the Rust engine's own unit
/// tests — fallback chains, on-disk wire compatibility, env loading, save/load round-trip.</para>
///
/// <para>These mutate process-global environment variables, so the class is not parallelised.</para>
/// </summary>
[Collection("providers-env")]
public class ProvidersTests : IDisposable
{
    private sealed record CorpusSlot(
        [property: JsonPropertyName("model")] string Model,
        [property: JsonPropertyName("apiUrl")] string ApiUrl,
        [property: JsonPropertyName("apiKey")] string ApiKey,
        [property: JsonPropertyName("apiFormat")] string ApiFormat,
        [property: JsonPropertyName("maxTokens")] int MaxTokens,
        [property: JsonPropertyName("temperature")] double Temperature);

    private sealed record CorpusPreset(
        [property: JsonPropertyName("name")] string Name,
        [property: JsonPropertyName("providerId")] string ProviderId,
        [property: JsonPropertyName("registeredProviders")] IReadOnlyList<string> RegisteredProviders,
        [property: JsonPropertyName("slots")] IReadOnlyDictionary<string, CorpusSlot> Slots);

    private sealed record CorpusFactory(
        [property: JsonPropertyName("factory")] string Factory,
        [property: JsonPropertyName("id")] string Id,
        [property: JsonPropertyName("apiUrl")] string ApiUrl,
        [property: JsonPropertyName("apiKey")] string ApiKey,
        [property: JsonPropertyName("apiFormat")] string ApiFormat,
        [property: JsonPropertyName("defaultModel")] string DefaultModel);

    private sealed record CorpusQuirk(
        [property: JsonPropertyName("upstream")] string Upstream,
        [property: JsonPropertyName("strictToolCallJson")] bool StrictToolCallJson,
        [property: JsonPropertyName("allowParallelTools")] bool? AllowParallelTools,
        [property: JsonPropertyName("matchedKeys")] IReadOnlyList<string> MatchedKeys);

    private sealed record CorpusUrl(
        [property: JsonPropertyName("apiUrl")] string ApiUrl,
        [property: JsonPropertyName("modelInfoUrl")] string ModelInfoUrl);

    private sealed record CorpusEntry(
        [property: JsonPropertyName("alias")] string Alias,
        [property: JsonPropertyName("upstream")] string? Upstream,
        [property: JsonPropertyName("id")] string? Id);

    private sealed record CorpusParse(
        [property: JsonPropertyName("id")] string Id,
        [property: JsonPropertyName("body")] string Body,
        [property: JsonPropertyName("entries")] IReadOnlyList<CorpusEntry> Entries);

    private sealed record CorpusName(
        [property: JsonPropertyName("name")] string Name,
        [property: JsonPropertyName("preset")] string? Preset);

    private sealed record CorpusSlotRef(
        [property: JsonPropertyName("provider")] string Provider,
        [property: JsonPropertyName("model")] string Model);

    private sealed record CorpusWire(
        [property: JsonPropertyName("id")] string Id,
        [property: JsonPropertyName("json")] string Json,
        [property: JsonPropertyName("slotModels")] IReadOnlyDictionary<string, string> SlotModels);

    private sealed record CorpusChain(
        [property: JsonPropertyName("apiUrl")] string ApiUrl,
        [property: JsonPropertyName("model")] string Model,
        [property: JsonPropertyName("apiKey")] string ApiKey);

    private sealed record RoutingCorpus(
        [property: JsonPropertyName("presetNames")] IReadOnlyList<CorpusName> PresetNames,
        [property: JsonPropertyName("defaultRouting")] IReadOnlyDictionary<string, CorpusSlotRef> DefaultRouting,
        [property: JsonPropertyName("wireCompat")] IReadOnlyList<CorpusWire> WireCompat,
        [property: JsonPropertyName("fallbackChain")] CorpusChain FallbackChain,
        [property: JsonPropertyName("unregisteredWithoutFallbackErrors")] bool UnregisteredWithoutFallbackErrors,
        [property: JsonPropertyName("presets")] IReadOnlyList<CorpusPreset> Presets,
        [property: JsonPropertyName("providerFactories")] IReadOnlyList<CorpusFactory> ProviderFactories,
        [property: JsonPropertyName("quirks")] IReadOnlyList<CorpusQuirk> Quirks,
        [property: JsonPropertyName("modelInfoUrls")] IReadOnlyList<CorpusUrl> ModelInfoUrls,
        [property: JsonPropertyName("modelInfoParse")] IReadOnlyList<CorpusParse> ModelInfoParse);

    /// <summary>The shared corpus, copied next to the test assembly by the csproj.</summary>
    private static readonly string CorpusPath = Path.Combine(AppContext.BaseDirectory, "providers", "routing.json");

    private static readonly RoutingCorpus Corpus =
        JsonSerializer.Deserialize<RoutingCorpus>(File.ReadAllText(CorpusPath))
        ?? throw new InvalidOperationException($"shared routing corpus did not parse: {CorpusPath}");

    private static readonly Dictionary<string, Activity> Activities = new(StringComparer.Ordinal)
    {
        ["coding"] = Activity.Coding,
        ["reasoning"] = Activity.Reasoning,
        ["reviewing"] = Activity.Reviewing,
        ["judge"] = Activity.Judge,
        ["summarize"] = Activity.Summarize,
        ["fast"] = Activity.Fast,
    };

    private readonly string? _priorGatewayUrl;

    public ProvidersTests()
    {
        // The corpus pins the production gateway URL, which only applies when
        // SMOOAI_GATEWAY_URL is ABSENT.
        _priorGatewayUrl = Environment.GetEnvironmentVariable("SMOOAI_GATEWAY_URL");
        Environment.SetEnvironmentVariable("SMOOAI_GATEWAY_URL", null);
    }

    public void Dispose() => Environment.SetEnvironmentVariable("SMOOAI_GATEWAY_URL", _priorGatewayUrl);

    private static LlmConfig Resolve(ProviderRegistry registry, string label) =>
        label == "default" ? registry.DefaultLlmConfig() : registry.LlmConfigFor(Activities[label]);

    public static TheoryData<string> PresetNames
    {
        get
        {
            var data = new TheoryData<string>();
            foreach (var p in Corpus.Presets)
            {
                data.Add(p.Name);
            }

            return data;
        }
    }

    [Fact]
    public void Corpus_CarriesAllFivePresets() => Assert.Equal(5, Corpus.Presets.Count);

    [Theory]
    [MemberData(nameof(PresetNames))]
    public void Preset_RoutingMatchesCorpus(string name)
    {
        var want = Corpus.Presets.Single(p => p.Name == name);
        var preset = Providers.PresetFromName(name);
        Assert.NotNull(preset);
        Assert.Equal(want.ProviderId, preset!.Value.ProviderId());

        var registry = ProviderRegistry.FromPreset(preset.Value, "test-key");
        Assert.Equal(want.RegisteredProviders, registry.ListProviders());

        foreach (var (label, slot) in want.Slots)
        {
            var config = Resolve(registry, label);
            Assert.Equal(slot.Model, config.Model);
            Assert.Equal(slot.ApiUrl, config.ApiUrl);
            Assert.Equal(slot.ApiKey, config.ApiKey);
            Assert.Equal(slot.ApiFormat, config.ApiFormat.ToString());
            Assert.Equal(slot.MaxTokens, config.MaxTokens);
            Assert.Equal(slot.Temperature, config.Temperature);
        }
    }

    [Fact]
    public void PresetNamesAndAliases_MatchCorpus()
    {
        foreach (var vector in Corpus.PresetNames)
        {
            var preset = Providers.PresetFromName(vector.Name);
            if (vector.Preset is null)
            {
                Assert.Null(preset);
            }
            else
            {
                Assert.NotNull(preset);
                Assert.Equal(vector.Preset, preset!.Value.ProviderId());
            }
        }
    }

    [Fact]
    public void ProviderFactories_MatchCorpus()
    {
        var factories = new Dictionary<string, ProviderConfig>(StringComparer.Ordinal)
        {
            ["openrouter"] = Providers.OpenRouter("k"),
            ["openai"] = Providers.OpenAI("k"),
            ["anthropic"] = Providers.Anthropic("k"),
            ["ollama"] = Providers.Ollama(),
            ["google"] = Providers.Google("k"),
            ["kimi"] = Providers.Kimi("k"),
            ["kimiCode"] = Providers.KimiCode("k"),
            ["llmgateway"] = Providers.LlmGateway("k"),
            ["smooaiGateway"] = Providers.SmooaiGateway("k"),
        };

        foreach (var want in Corpus.ProviderFactories)
        {
            var got = factories[want.Factory];
            Assert.Equal(want.Id, got.Id);
            Assert.Equal(want.ApiUrl, got.ApiUrl);
            Assert.Equal(want.ApiKey, got.ApiKey);
            Assert.Equal(want.ApiFormat, got.ApiFormat.ToString());
            Assert.Equal(want.DefaultModel, got.DefaultModel);
        }
    }

    [Fact]
    public void DefaultRouting_IsProviderAgnostic()
    {
        var routing = ModelRouting.Neutral();
        foreach (var (label, want) in Corpus.DefaultRouting)
        {
            var slot = label == "default" ? routing.Default : routing.SlotFor(Activities[label]);
            Assert.Equal(want.Provider, slot.Provider);
            Assert.Equal(want.Model, slot.Model);
        }

        // The hosted gateway is opt-in, never the default.
        Assert.NotEqual("smooai-gateway", routing.Coding.Provider);
    }

    [Fact]
    public void OnDiskWireCompat_MigratesLegacySlotNames()
    {
        foreach (var vector in Corpus.WireCompat)
        {
            var registry = ProviderRegistry.FromJson(vector.Json);
            foreach (var (label, want) in vector.SlotModels)
            {
                var slot = label == "default" ? registry.Routing.Default : registry.Routing.SlotFor(Activities[label]);
                Assert.Equal(want, slot.Model);
            }
        }
    }

    [Fact]
    public void FallbackChain_ResolvesToTheRegisteredProvider()
    {
        var registry = new ProviderRegistry();
        registry.RegisterProvider(new ProviderConfig("tertiary", "https://tertiary.example.com/v1", "t-key", ApiFormat.OpenAiCompat, "model-c"));
        registry.Routing = registry.Routing with
        {
            Coding = new ModelSlot("primary", "model-a")
                .WithFallback(new ModelSlot("secondary", "model-b").WithFallback(new ModelSlot("tertiary", "model-c"))),
        };

        var config = registry.LlmConfigFor(Activity.Coding);
        Assert.Equal(Corpus.FallbackChain.ApiUrl, config.ApiUrl);
        Assert.Equal(Corpus.FallbackChain.Model, config.Model);
        Assert.Equal(Corpus.FallbackChain.ApiKey, config.ApiKey);
    }

    [Fact]
    public void UnregisteredWithoutFallback_Throws()
    {
        Assert.True(Corpus.UnregisteredWithoutFallbackErrors);
        var registry = new ProviderRegistry();
        registry.Routing = registry.Routing with { Coding = new ModelSlot("nope", "m") };
        var ex = Assert.Throws<InvalidOperationException>(() => registry.LlmConfigFor(Activity.Coding));
        Assert.Contains("not registered", ex.Message, StringComparison.Ordinal);
    }

    [Fact]
    public void Quirks_MatchCorpus()
    {
        foreach (var vector in Corpus.Quirks)
        {
            var quirks = Providers.QuirksForModel(vector.Upstream);
            Assert.Equal(vector.StrictToolCallJson, quirks.StrictToolCallJson);
            Assert.Equal(vector.AllowParallelTools, quirks.AllowParallelTools);
            Assert.Equal(vector.MatchedKeys, Providers.QuirksDebugSnapshot(vector.Upstream).Keys.OrderBy(k => k, StringComparer.Ordinal).ToList());
        }
    }

    [Fact]
    public void ModelInfoUrls_MatchCorpus()
    {
        foreach (var vector in Corpus.ModelInfoUrls)
        {
            Assert.Equal(vector.ModelInfoUrl, Providers.BuildModelInfoUrl(vector.ApiUrl));
        }
    }

    [Fact]
    public void ModelInfoParse_MatchesCorpus()
    {
        foreach (var vector in Corpus.ModelInfoParse)
        {
            var parsed = Providers.ParseModelInfo(vector.Body);
            Assert.Equal(vector.Entries.Select(e => e.Alias).ToList(), parsed.Keys.ToList());
            foreach (var want in vector.Entries)
            {
                var entry = parsed[want.Alias];
                Assert.Equal(want.Upstream, entry.Upstream);
                Assert.Equal(want.Id, entry.Id);
            }
        }
    }

    [Fact]
    public void ParseModelInfo_RejectsBadBodies()
    {
        Assert.Throws<InvalidOperationException>(() => Providers.ParseModelInfo("not json"));
        Assert.Throws<InvalidOperationException>(() => Providers.ParseModelInfo("{\"nope\":1}"));
    }

    [Fact]
    public void Registry_WritesTheShapeRustReads()
    {
        var dir = Path.Combine(Path.GetTempPath(), Path.GetRandomFileName());
        var path = Path.Combine(dir, "nested", "providers.json");
        try
        {
            var registry = new ProviderRegistry();
            registry.RegisterProvider(Providers.OpenRouter("or-key"));
            registry.RegisterProvider(Providers.OpenAI("oai-key"));
            registry.SaveToFile(path);

            using var doc = JsonDocument.Parse(File.ReadAllText(path));
            var provider = doc.RootElement.GetProperty("providers")[0];
            foreach (var key in new[] { "id", "api_url", "api_key", "api_format", "default_model" })
            {
                Assert.True(provider.TryGetProperty(key, out _), $"provider entry is missing the on-disk key {key} — Rust will not read this file");
            }

            var routing = doc.RootElement.GetProperty("routing");
            foreach (var key in new[] { "coding", "reasoning", "reviewing", "judge", "summarize", "default", "fast" })
            {
                Assert.True(routing.TryGetProperty(key, out _), $"routing is missing the on-disk key {key}");
            }

            // `planning` and `thinking` are legacy: accepted on read, never written back.
            Assert.False(routing.TryGetProperty("planning", out _));
            Assert.False(routing.TryGetProperty("thinking", out _));
            // A slot with no fallback omits the key — `"fallback": null` is a different document.
            Assert.False(routing.GetProperty("coding").TryGetProperty("fallback", out _));
            // api_format serializes as the Rust variant name, not an integer.
            Assert.Equal("OpenAiCompat", provider.GetProperty("api_format").GetString());

            var loaded = ProviderRegistry.LoadFromFile(path);
            Assert.Equal(new[] { "openai", "openrouter" }, loaded.ListProviders());
            Assert.Equal("or-key", loaded.GetProvider("openrouter")?.ApiKey);
            var config = loaded.LlmConfigFor(Activity.Reasoning);
            Assert.Equal("openrouter/auto", config.Model);
            Assert.Equal("or-key", config.ApiKey);
        }
        finally
        {
            if (Directory.Exists(dir))
            {
                Directory.Delete(dir, recursive: true);
            }
        }
    }

    [Fact]
    public void FallbackChain_SurvivesJsonRoundTrip()
    {
        var registry = ProviderRegistry.FromPreset(Preset.OpenRouterLowCost, "k");
        var restored = ProviderRegistry.FromJson(registry.ToJson());
        Assert.Equal("minimax/minimax-m2.5", restored.Routing.Coding.Fallback?.Model);
        Assert.Equal("minimax/minimax-m2.7", restored.LlmConfigFor(Activity.Coding).Model);
    }

    [Fact]
    public void FromEnv_ReadsProviderAndModel()
    {
        var priorKey = Environment.GetEnvironmentVariable("SMOOTH_API_KEY");
        var priorProvider = Environment.GetEnvironmentVariable("SMOOTH_PROVIDER");
        var priorModel = Environment.GetEnvironmentVariable("SMOOTH_MODEL");
        try
        {
            Environment.SetEnvironmentVariable("SMOOTH_API_KEY", "env-test-key");
            Environment.SetEnvironmentVariable("SMOOTH_PROVIDER", "openai");
            Environment.SetEnvironmentVariable("SMOOTH_MODEL", null);

            var registry = ProviderRegistry.FromEnv();
            Assert.NotNull(registry);
            Assert.Equal("env-test-key", registry!.GetProvider("openai")?.ApiKey);
            Assert.Equal("gpt-4o", registry.DefaultLlmConfig().Model);

            Environment.SetEnvironmentVariable("SMOOTH_MODEL", "gpt-4o-mini");
            Assert.Equal("gpt-4o-mini", ProviderRegistry.FromEnv()!.DefaultLlmConfig().Model);
        }
        finally
        {
            Environment.SetEnvironmentVariable("SMOOTH_API_KEY", priorKey);
            Environment.SetEnvironmentVariable("SMOOTH_PROVIDER", priorProvider);
            Environment.SetEnvironmentVariable("SMOOTH_MODEL", priorModel);
        }
    }

    [Fact]
    public void FromEnv_RequiresAKey()
    {
        var prior = Environment.GetEnvironmentVariable("SMOOTH_API_KEY");
        try
        {
            Environment.SetEnvironmentVariable("SMOOTH_API_KEY", null);
            Assert.Null(ProviderRegistry.FromEnv());
        }
        finally
        {
            Environment.SetEnvironmentVariable("SMOOTH_API_KEY", prior);
        }
    }

    [Fact]
    public void SmooaiGateway_RespectsUrlOverride()
    {
        Environment.SetEnvironmentVariable("SMOOAI_GATEWAY_URL", "https://llm.dev.smooai.com/v1");
        try
        {
            var config = ProviderRegistry.FromPreset(Preset.SmoaiGateway, "dev-key").DefaultLlmConfig();
            Assert.Equal("https://llm.dev.smooai.com/v1", config.ApiUrl);
            Assert.Equal("dev-key", config.ApiKey);
        }
        finally
        {
            Environment.SetEnvironmentVariable("SMOOAI_GATEWAY_URL", null);
        }
    }

    [Fact]
    public void SetDefaultProvider_ThenRemove()
    {
        var registry = new ProviderRegistry();
        registry.RegisterProvider(Providers.Kimi("k-key"));
        registry.SetDefaultProvider("kimi");

        foreach (var activity in Activities.Values)
        {
            var config = registry.LlmConfigFor(activity);
            Assert.Equal("kimi-k2.5", config.Model);
            Assert.Equal("https://api.moonshot.ai/v1", config.ApiUrl);
        }

        registry.RemoveProvider("kimi");
        Assert.Throws<InvalidOperationException>(() => registry.LlmConfigFor(Activity.Coding));
    }

    [Fact]
    public void RecommendedPresetIsListedFirst()
    {
        Assert.Equal("smooai-gateway", Providers.AllPresets[0].Name);
        Assert.Contains("recommended", Providers.AllPresets[0].Label, StringComparison.Ordinal);
        Assert.Equal(5, Providers.AllPresets.Count);
    }

    /// <summary>
    /// The integration point: a resolved route becomes a live client. An Anthropic-dialect provider
    /// must be refused, not spoken to in OpenAI's wire format.
    /// </summary>
    [Fact]
    public void ClientFor_RefusesANonOpenAiDialect()
    {
        var (client, config) = ProviderRegistry.FromPreset(Preset.OpenAI, "k").ClientFor(Activity.Coding);
        using (client)
        {
            Assert.Equal("gpt-4o", config.Model);
        }

        var ex = Assert.Throws<InvalidOperationException>(() => ProviderRegistry.FromPreset(Preset.Anthropic, "k").ClientFor(Activity.Coding));
        Assert.Contains("cannot speak", ex.Message, StringComparison.Ordinal);
    }

    [Fact]
    public void ToString_NeverLeaksTheApiKey()
    {
        Assert.DoesNotContain("super-secret-key", Providers.OpenRouter("super-secret-key").ToString(), StringComparison.Ordinal);
        var config = ProviderRegistry.FromPreset(Preset.OpenAI, "super-secret-key").LlmConfigFor(Activity.Coding);
        Assert.DoesNotContain("super-secret-key", config.ToString(), StringComparison.Ordinal);
    }

    [Fact]
    public void SlotFor_FallsBackToDefault()
    {
        // A partial table — no reasoning, no fast — still resolves both slots.
        var slot = new ModelSlot("p", "m-default");
        var routing = new ModelRouting(new ModelSlot("p", "m-coding"), slot, slot, slot, slot);
        Assert.Equal("m-default", routing.SlotFor(Activity.Reasoning).Model);
        Assert.Equal("m-default", routing.SlotFor(Activity.Fast).Model);
        Assert.Equal("m-coding", routing.SlotFor(Activity.Coding).Model);
    }
}

/// <summary>Serialises the env-mutating provider tests so they cannot race each other.</summary>
[CollectionDefinition("providers-env", DisableParallelization = true)]
public class ProvidersEnvCollection;
