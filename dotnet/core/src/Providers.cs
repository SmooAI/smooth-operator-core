using System.Net.Http.Headers;
using System.Text.Json;
using System.Text.Json.Serialization;

namespace SmooAI.SmoothOperator.Core;

/// <summary>
/// The wire dialect a provider speaks. The serialized names match the Rust reference's serde output
/// so <c>providers.json</c> round-trips between engines.
/// </summary>
public enum ApiFormat
{
    /// <summary>The OpenAI <c>/chat/completions</c> dialect.</summary>
    OpenAiCompat,

    /// <summary>Anthropic's native <c>/messages</c> dialect.</summary>
    Anthropic,
}

/// <summary>Connection detail for a single LLM provider.</summary>
/// <param name="Id">Provider id, e.g. <c>openrouter</c>.</param>
/// <param name="ApiUrl">OpenAI-compatible base URL.</param>
/// <param name="ApiKey">Bearer key. Never printed — see <see cref="ToString"/>.</param>
/// <param name="ApiFormat">The wire dialect.</param>
/// <param name="DefaultModel">Model used when a slot does not name one.</param>
public sealed record ProviderConfig(string Id, string ApiUrl, string ApiKey, ApiFormat ApiFormat, string DefaultModel)
{
    /// <summary>
    /// Redacts the API key so it never lands in logs, exception messages or crash dumps. Everything
    /// else is printed verbatim, mirroring the Rust reference's manual <c>Debug</c> impl.
    /// </summary>
    public override string ToString() =>
        $"ProviderConfig {{ Id = {Id}, ApiUrl = {ApiUrl}, ApiKey = ***redacted***, ApiFormat = {ApiFormat}, DefaultModel = {DefaultModel} }}";
}

/// <summary>Ready-made provider + routing configurations.</summary>
public enum Preset
{
    /// <summary>The hosted Smoo AI gateway — the recommended default.</summary>
    SmoaiGateway,

    /// <summary>Chinese frontier models via OpenRouter — the cheapest option.</summary>
    OpenRouterLowCost,

    /// <summary>Chinese frontier models via LLM Gateway.</summary>
    LlmGatewayLowCost,

    /// <summary>OpenAI models.</summary>
    OpenAI,

    /// <summary>Anthropic Claude models.</summary>
    Anthropic,
}

/// <summary>One row of <see cref="Providers.AllPresets"/>: CLI name, display label, description.</summary>
public sealed record PresetInfo(string Name, string Label, string Description);

/// <summary>
/// Selects which model slot a call routes through. Six semantic slots: the legacy Thinking +
/// Planning split collapsed into <see cref="Reasoning"/>, and the legacy "default" alias is served
/// by <see cref="Coding"/>.
/// </summary>
public enum Activity
{
    /// <summary>The outer coding loop — the workhorse slot, which also serves the legacy "default" call path.</summary>
    Coding,

    /// <summary>Deep reasoning / planning / chain-of-thought.</summary>
    Reasoning,

    /// <summary>Code review, critique, adversarial checks.</summary>
    Reviewing,

    /// <summary>LLM-as-a-judge: yes/no verdicts, low latency, used by Narc guardrails and bench scoring.</summary>
    Judge,

    /// <summary>Context compression during long agent runs.</summary>
    Summarize,

    /// <summary>
    /// Small, latency-sensitive utility calls: session auto-naming, short titles, autocomplete.
    /// Sub-second first token, short output, no tool use — don't pay Sonnet-plus prices to name a session.
    /// </summary>
    Fast,
}

/// <summary>A provider id + model name, with an optional fallback used when the provider is not registered.</summary>
public sealed record ModelSlot(string Provider, string Model, ModelSlot? Fallback = null)
{
    /// <summary>Return a copy of this slot with <paramref name="fallback"/> attached.</summary>
    public ModelSlot WithFallback(ModelSlot fallback) => this with { Fallback = fallback };
}

/// <summary>
/// The per-activity routing table.
///
/// <para>Six semantic slots plus a <see cref="Default"/> slot kept for wire compatibility: no
/// <see cref="Activity"/> routes through it directly (<see cref="Activity.Coding"/> serves the
/// default path), but the field stays so pre-collapse configs load cleanly.</para>
/// </summary>
public sealed record ModelRouting(
    ModelSlot Coding,
    ModelSlot Reviewing,
    ModelSlot Judge,
    ModelSlot Summarize,
    ModelSlot Default,
    ModelSlot? Reasoning = null,
    ModelSlot? Fast = null,
    ModelSlot? Planning = null)
{
    /// <summary>
    /// The slot for an activity. <see cref="Activity.Reasoning"/> and <see cref="Activity.Fast"/>
    /// fall back to <see cref="Default"/> when absent, so partial configs stay functional.
    /// </summary>
    public ModelSlot SlotFor(Activity activity) => activity switch
    {
        Activity.Coding => Coding,
        Activity.Reasoning => Reasoning ?? Default,
        Activity.Reviewing => Reviewing,
        Activity.Judge => Judge,
        Activity.Summarize => Summarize,
        Activity.Fast => Fast ?? Default,
        _ => Default,
    };

    /// <summary>
    /// The neutral, provider-agnostic routing every slot starts on: the well-known
    /// <c>openrouter</c> provider id with a placeholder <c>auto</c> model, so the library ships no
    /// opinion about a specific hosted gateway. Consumers opt into the Smoo AI gateway via
    /// <see cref="Preset.SmoaiGateway"/> explicitly.
    /// </summary>
    public static ModelRouting Neutral() => Uniform(new ModelSlot("openrouter", "openrouter/auto"));

    /// <summary>Point every slot at the same target.</summary>
    public static ModelRouting Uniform(ModelSlot slot) =>
        new(slot, slot, slot, slot, slot, Reasoning: slot, Fast: slot);
}

/// <summary>
/// A fully resolved route: the provider connection plus the model the activity picked. Feed
/// <see cref="ApiUrl"/>/<see cref="ApiKey"/>/<see cref="Model"/> to <see cref="GatewayChatClient"/>.
/// </summary>
public sealed record LlmConfig(string ApiUrl, string ApiKey, string Model, int MaxTokens, double Temperature, ApiFormat ApiFormat)
{
    /// <summary>Redacts the API key, same as <see cref="ProviderConfig.ToString"/>.</summary>
    public override string ToString() =>
        $"LlmConfig {{ ApiUrl = {ApiUrl}, ApiKey = ***redacted***, Model = {Model}, MaxTokens = {MaxTokens}, Temperature = {Temperature}, ApiFormat = {ApiFormat} }}";
}

/// <summary>One routing entry returned by a gateway's <c>/model/info</c>.</summary>
/// <param name="Alias">The name callers use (e.g. <c>smooth-coding</c>).</param>
/// <param name="Upstream">The concrete model, when the gateway chose to surface it.</param>
/// <param name="Id">Stable id from <c>model_info.id</c>, useful for tracing a rename.</param>
public sealed record ResolvedModel(string Alias, string? Upstream, string? Id);

/// <summary>
/// Per-model wire-format flags. Populate a field only when the quirk is worth the branch — every
/// conditional is a place for drift.
///
/// <para>When routing through a LiteLLM-style gateway the concrete upstream model only reveals
/// itself in response headers (<c>x-litellm-model-name</c>), by which point the request is already
/// sent. So prefer always-safe request shapes over per-model conditionals, and keep this table for
/// the cases where the strict form does not work everywhere.</para>
/// </summary>
/// <param name="AllowParallelTools">When non-null and false, force <c>parallel_tool_calls</c> off even if the agent config requests it.</param>
/// <param name="StrictToolCallJson">Ask the client to be extra careful about tool_call echo shape. Nothing reads this yet.</param>
public sealed record ModelQuirks(bool? AllowParallelTools = null, bool StrictToolCallJson = false);

/// <summary>
/// Provider routing — the .NET port of the Rust reference engine's <c>providers.rs</c>,
/// <c>quirks.rs</c> and <c>resolution.rs</c>.
///
/// <para>Three concerns, one type because they are one story: <b>which</b> model a given activity
/// should use, <b>what</b> wire quirks that concrete model has, and — when the route points at a
/// LiteLLM-style gateway — <b>which</b> upstream model a semantic alias actually resolves to.</para>
///
/// <para>Routing values are pinned across all five engines by the shared corpus at
/// <c>spec/providers/routing.json</c> — a slot that resolves to the wrong model or base URL sends
/// real traffic and real money somewhere nobody intended, and it looks like it is working.</para>
/// </summary>
public static class Providers
{
    /// <summary>OpenRouter — an OpenAI-compatible proxy for many models.</summary>
    public static ProviderConfig OpenRouter(string apiKey) =>
        new("openrouter", "https://openrouter.ai/api/v1", apiKey, ApiFormat.OpenAiCompat, "openai/gpt-4o");

    /// <summary>The OpenAI direct API.</summary>
    public static ProviderConfig OpenAI(string apiKey) =>
        new("openai", "https://api.openai.com/v1", apiKey, ApiFormat.OpenAiCompat, "gpt-4o");

    /// <summary>The Anthropic native API.</summary>
    public static ProviderConfig Anthropic(string apiKey) =>
        new("anthropic", "https://api.anthropic.com/v1", apiKey, ApiFormat.Anthropic, "claude-sonnet-4-20250514");

    /// <summary>A local Ollama instance — no API key needed.</summary>
    public static ProviderConfig Ollama() =>
        new("ollama", "http://localhost:11434/v1", string.Empty, ApiFormat.OpenAiCompat, "llama3");

    /// <summary>The Google Gemini API (OpenAI-compatible surface).</summary>
    public static ProviderConfig Google(string apiKey) =>
        new("google", "https://generativelanguage.googleapis.com/v1beta/openai", apiKey, ApiFormat.OpenAiCompat, "gemini-2.0-flash");

    /// <summary>Moonshot AI's general-purpose API (OpenAI-compatible).</summary>
    public static ProviderConfig Kimi(string apiKey) =>
        new("kimi", "https://api.moonshot.ai/v1", apiKey, ApiFormat.OpenAiCompat, "kimi-k2.5");

    /// <summary>Moonshot's coding-optimized API (Anthropic-compatible).</summary>
    public static ProviderConfig KimiCode(string apiKey) =>
        new("kimi-code", "https://api.kimi.com/coding/v1", apiKey, ApiFormat.Anthropic, "kimi-for-coding");

    /// <summary>LLM Gateway — a unified API for 210+ models.</summary>
    public static ProviderConfig LlmGateway(string apiKey) =>
        new("llmgateway", "https://api.llmgateway.io/v1", apiKey, ApiFormat.OpenAiCompat, "openai/gpt-4o");

    /// <summary>
    /// The hosted LiteLLM-backed gateway run by Smoo AI.
    ///
    /// <para>One API key, one URL, OpenAI-compatible. The gateway handles provider selection,
    /// billing, moderation and cost tracking server-side, so consumers reference models by semantic
    /// aliases (<c>smooth-coding</c>, <c>smooth-judge</c>, …) that the gateway maps to whichever
    /// underlying model is currently best — upgrades ship server-side with no client release.</para>
    ///
    /// <para><c>SMOOAI_GATEWAY_URL</c> overrides the base URL. Only an ABSENT variable takes the
    /// default: a set-but-empty override yields an empty base URL, matching Rust.</para>
    /// </summary>
    public static ProviderConfig SmooaiGateway(string apiKey) =>
        new("smooai-gateway", Environment.GetEnvironmentVariable("SMOOAI_GATEWAY_URL") ?? "https://llm.smoo.ai/v1", apiKey, ApiFormat.OpenAiCompat, "smooth-default");

    /// <summary>
    /// Every preset. The first entry is the recommended default — <c>th auth login</c> shows them in
    /// this order.
    /// </summary>
    public static IReadOnlyList<PresetInfo> AllPresets { get; } =
    [
        new("smooai-gateway", "Smoo AI Gateway (recommended)",
            "Hosted LiteLLM gateway run by Smoo AI — billing, moderation, governance, 100+ models. One key, one URL, no config."),
        new("openrouter-low-cost", "OpenRouter Low Cost",
            "GLM-5.1 thinking (#1 SWE-Bench Pro), MiniMax-M2.7 coding (56% SWE-Pro, 10B params), DeepSeek-V3.2 default"),
        new("llmgateway-low-cost", "LLM Gateway Low Cost",
            "GLM-5 thinking, MiniMax-M2.7 coding, DeepSeek-V3.2 default — unified billing, 224 models"),
        new("openai", "OpenAI", "o3-mini thinking, GPT-4o coding — OpenAI ecosystem"),
        new("anthropic", "Anthropic", "Claude Opus thinking, Sonnet coding — highest quality"),
    ];

    /// <summary>Parse a preset name or alias. Returns <c>null</c> for unknown names.</summary>
    public static Preset? PresetFromName(string name) => name switch
    {
        "smooai-gateway" or "smooai" or "gateway" => Preset.SmoaiGateway,
        "openrouter-low-cost" or "low-cost" => Preset.OpenRouterLowCost,
        "llmgateway-low-cost" or "gateway-low-cost" => Preset.LlmGatewayLowCost,
        "openai" or "codex" => Preset.OpenAI,
        "anthropic" => Preset.Anthropic,
        _ => null,
    };

    /// <summary>The provider id a preset requires.</summary>
    public static string ProviderId(this Preset preset) => preset switch
    {
        Preset.SmoaiGateway => "smooai-gateway",
        Preset.OpenRouterLowCost => "openrouter",
        Preset.LlmGatewayLowCost => "llmgateway",
        Preset.OpenAI => "openai",
        Preset.Anthropic => "anthropic",
        _ => string.Empty,
    };

    // ── quirks ─────────────────────────────────────────────────────────────

    private static readonly (string Needle, ModelQuirks Quirks)[] QuirksTable =
    [
        ("qwen3-coder", new ModelQuirks(StrictToolCallJson: true)),
        ("qwen-coder", new ModelQuirks(StrictToolCallJson: true)),
    ];

    /// <summary>
    /// Look up quirks by concrete upstream name. Matching is case-insensitive and substring-based,
    /// so minor version drift (<c>qwen3-coder-plus-2025-04</c>) still hits the <c>qwen3-coder</c>
    /// entry. Returns safe defaults when nothing matches.
    /// </summary>
    public static ModelQuirks QuirksForModel(string upstream)
    {
        var lowered = upstream.ToLowerInvariant();
        foreach (var (needle, quirks) in QuirksTable)
        {
            if (lowered.Contains(needle, StringComparison.Ordinal))
            {
                return quirks;
            }
        }

        return new ModelQuirks();
    }

    /// <summary>The quirk table's canonical keys, for diagnostics.</summary>
    public static IReadOnlyList<string> QuirkKeys() => QuirksTable.Select(e => e.Needle).ToList();

    /// <summary>Every quirk entry matching an upstream name. Usually one wins; the full set is kept for tests.</summary>
    public static IReadOnlyDictionary<string, ModelQuirks> QuirksDebugSnapshot(string upstream)
    {
        var lowered = upstream.ToLowerInvariant();
        return QuirksTable
            .Where(e => lowered.Contains(e.Needle, StringComparison.Ordinal))
            .ToDictionary(e => e.Needle, e => e.Quirks, StringComparer.Ordinal);
    }

    // ── alias resolution ───────────────────────────────────────────────────

    /// <summary>
    /// Derive the <c>/model/info</c> URL from a provider's OpenAI-compat api url (e.g.
    /// <c>https://llm.smoo.ai/v1</c>). Stripping <c>/v1</c> is safe: <c>/model/info</c> lives at the
    /// gateway root in every LiteLLM deployment seen.
    /// </summary>
    public static string BuildModelInfoUrl(string apiUrl)
    {
        var trimmed = apiUrl.TrimEnd('/');
        var expectedBase = trimmed.EndsWith("/v1", StringComparison.Ordinal) ? trimmed[..^"/v1".Length] : trimmed;
        return expectedBase + "/model/info";
    }

    private sealed record ModelInfoDoc(
        [property: JsonPropertyName("data")] IReadOnlyList<ModelInfoEntry>? Data);

    private sealed record ModelInfoEntry(
        [property: JsonPropertyName("model_name")] string ModelName,
        [property: JsonPropertyName("litellm_params")] LiteLlmParams? LitellmParams,
        [property: JsonPropertyName("model_info")] ModelInfoField? ModelInfo);

    private sealed record LiteLlmParams([property: JsonPropertyName("model")] string? Model);

    private sealed record ModelInfoField([property: JsonPropertyName("id")] string? Id);

    /// <summary>
    /// Parse a <c>/model/info</c> response body into an alias → entry map, ordinal-sorted by alias so
    /// diagnostics print the same order every run (Rust returns a <c>BTreeMap</c>).
    /// </summary>
    /// <exception cref="InvalidOperationException">The body is not valid JSON or is missing <c>data</c>.</exception>
    public static SortedDictionary<string, ResolvedModel> ParseModelInfo(string body)
    {
        ModelInfoDoc? doc;
        try
        {
            doc = JsonSerializer.Deserialize<ModelInfoDoc>(body);
        }
        catch (JsonException ex)
        {
            throw new InvalidOperationException($"parsing /model/info response: {ex.Message}", ex);
        }

        if (doc?.Data is null)
        {
            throw new InvalidOperationException("parsing /model/info response: missing `data` array");
        }

        var map = new SortedDictionary<string, ResolvedModel>(StringComparer.Ordinal);
        foreach (var entry in doc.Data)
        {
            map[entry.ModelName] = new ResolvedModel(entry.ModelName, entry.LitellmParams?.Model, entry.ModelInfo?.Id);
        }

        return map;
    }

    /// <summary>
    /// Ask a LiteLLM gateway for its alias → upstream map.
    ///
    /// <para>A 401 means the provider's API key is missing or rejected; either way the caller cannot
    /// see the mapping.</para>
    /// </summary>
    public static async Task<SortedDictionary<string, ResolvedModel>> FetchModelInfoAsync(
        string apiUrl,
        string apiKey,
        HttpClient? httpClient = null,
        CancellationToken cancellationToken = default)
    {
        var url = BuildModelInfoUrl(apiUrl);
        var http = httpClient ?? new HttpClient { Timeout = TimeSpan.FromSeconds(10) };
        try
        {
            using var request = new HttpRequestMessage(HttpMethod.Get, url);
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", apiKey);
            using var response = await http.SendAsync(request, cancellationToken).ConfigureAwait(false);
            var body = await response.Content.ReadAsStringAsync(cancellationToken).ConfigureAwait(false);
            if (!response.IsSuccessStatusCode)
            {
                throw new InvalidOperationException($"GET {url} returned {(int)response.StatusCode}: {body}");
            }

            return ParseModelInfo(body);
        }
        finally
        {
            if (httpClient is null)
            {
                http.Dispose();
            }
        }
    }
}

// ── on-disk wire shape ─────────────────────────────────────────────────────
// Shared byte-for-byte with the Rust CLI (~/.smooth/providers.json): the same file
// is written by one engine and read by another, so these names are snake_case and
// optional slots are omitted rather than written as null.

internal sealed record SlotWire(
    [property: JsonPropertyName("provider")] string Provider,
    [property: JsonPropertyName("model")] string Model,
    [property: JsonPropertyName("fallback"), JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] SlotWire? Fallback);

internal sealed record RoutingWire(
    [property: JsonPropertyName("coding")] SlotWire Coding,
    [property: JsonPropertyName("reasoning"), JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] SlotWire? Reasoning,
    [property: JsonPropertyName("thinking"), JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] SlotWire? Thinking,
    [property: JsonPropertyName("reviewing")] SlotWire Reviewing,
    [property: JsonPropertyName("judge")] SlotWire Judge,
    [property: JsonPropertyName("summarize")] SlotWire Summarize,
    [property: JsonPropertyName("default")] SlotWire Default,
    [property: JsonPropertyName("fast"), JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] SlotWire? Fast,
    [property: JsonPropertyName("planning"), JsonIgnore(Condition = JsonIgnoreCondition.WhenWritingNull)] SlotWire? Planning);

internal sealed record ProviderWire(
    [property: JsonPropertyName("id")] string Id,
    [property: JsonPropertyName("api_url")] string ApiUrl,
    [property: JsonPropertyName("api_key")] string ApiKey,
    [property: JsonPropertyName("api_format")] string ApiFormat,
    [property: JsonPropertyName("default_model")] string DefaultModel);

internal sealed record RegistryWire(
    [property: JsonPropertyName("providers")] IReadOnlyList<ProviderWire> Providers,
    [property: JsonPropertyName("routing")] RoutingWire Routing);

/// <summary>Registered providers plus the per-activity routing table.</summary>
public sealed class ProviderRegistry
{
    private readonly Dictionary<string, ProviderConfig> _providers = new(StringComparer.Ordinal);

    /// <summary>The per-activity table. Reassign it to re-point routes.</summary>
    public ModelRouting Routing { get; set; } = ModelRouting.Neutral();

    /// <summary>
    /// A registry pre-configured with a preset: registers the preset's provider and installs routing
    /// tuned for the preset's goals (cost, quality, latency).
    /// </summary>
    public static ProviderRegistry FromPreset(Preset preset, string apiKey)
    {
        var registry = new ProviderRegistry();
        switch (preset)
        {
            case Preset.SmoaiGateway:
                // Semantic aliases the gateway's LiteLLM config maps to whichever underlying model is
                // currently best. Changing the underlying model is a server-side deploy.
                registry.RegisterProvider(Providers.SmooaiGateway(apiKey));
                registry.Routing = new ModelRouting(
                    Coding: new ModelSlot("smooai-gateway", "smooth-coding"),
                    Reviewing: new ModelSlot("smooai-gateway", "smooth-reviewing"),
                    Judge: new ModelSlot("smooai-gateway", "smooth-judge"),
                    Summarize: new ModelSlot("smooai-gateway", "smooth-summarize"),
                    Default: new ModelSlot("smooai-gateway", "smooth-default"),
                    Reasoning: new ModelSlot("smooai-gateway", "smooth-reasoning"),
                    Fast: new ModelSlot("smooai-gateway", "smooth-fast"));
                break;
            case Preset.OpenRouterLowCost:
                // OpenRouter uses provider-prefixed model IDs.
                registry.RegisterProvider(Providers.OpenRouter(apiKey));
                registry.Routing = new ModelRouting(
                    Coding: new ModelSlot("openrouter", "minimax/minimax-m2.7").WithFallback(new ModelSlot("openrouter", "minimax/minimax-m2.5")),
                    Reviewing: new ModelSlot("openrouter", "deepseek/deepseek-v3.2"),
                    Judge: new ModelSlot("openrouter", "google/gemini-2.5-flash"),
                    Summarize: new ModelSlot("openrouter", "deepseek/deepseek-v3.2"),
                    Default: new ModelSlot("openrouter", "deepseek/deepseek-v3.2"),
                    Reasoning: new ModelSlot("openrouter", "z-ai/glm-5.1"),
                    Fast: new ModelSlot("openrouter", "google/gemini-2.5-flash-lite"));
                break;
            case Preset.LlmGatewayLowCost:
                // LLM Gateway uses bare model names.
                registry.RegisterProvider(Providers.LlmGateway(apiKey));
                registry.Routing = new ModelRouting(
                    Coding: new ModelSlot("llmgateway", "minimax-m2.7").WithFallback(new ModelSlot("llmgateway", "minimax-m2.5")),
                    Reviewing: new ModelSlot("llmgateway", "deepseek-v3.2"),
                    Judge: new ModelSlot("llmgateway", "gemini-2.5-flash"),
                    Summarize: new ModelSlot("llmgateway", "deepseek-v3.2"),
                    Default: new ModelSlot("llmgateway", "deepseek-v3.2"),
                    Reasoning: new ModelSlot("llmgateway", "glm-5"),
                    Fast: new ModelSlot("llmgateway", "gemini-2.5-flash-lite"));
                break;
            case Preset.OpenAI:
                registry.RegisterProvider(Providers.OpenAI(apiKey));
                registry.Routing = new ModelRouting(
                    Coding: new ModelSlot("openai", "gpt-4o"),
                    Reviewing: new ModelSlot("openai", "gpt-4o"),
                    Judge: new ModelSlot("openai", "gpt-4o-mini"),
                    Summarize: new ModelSlot("openai", "gpt-4o-mini"),
                    Default: new ModelSlot("openai", "gpt-4o"),
                    Reasoning: new ModelSlot("openai", "o3-mini"),
                    Fast: new ModelSlot("openai", "gpt-4o-mini"));
                break;
            case Preset.Anthropic:
                registry.RegisterProvider(Providers.Anthropic(apiKey));
                registry.Routing = new ModelRouting(
                    Coding: new ModelSlot("anthropic", "claude-sonnet-4-20250514"),
                    Reviewing: new ModelSlot("anthropic", "claude-sonnet-4-20250514"),
                    Judge: new ModelSlot("anthropic", "claude-haiku-4-5-20251001"),
                    Summarize: new ModelSlot("anthropic", "claude-haiku-4-5-20251001"),
                    Default: new ModelSlot("anthropic", "claude-sonnet-4-20250514"),
                    Reasoning: new ModelSlot("anthropic", "claude-opus-4-20250514"),
                    Fast: new ModelSlot("anthropic", "claude-haiku-4-5-20251001"));
                break;
        }

        return registry;
    }

    /// <summary>
    /// A minimal registry from <c>SMOOTH_API_KEY</c> (required), <c>SMOOTH_PROVIDER</c> (defaults to
    /// <c>openrouter</c>) and <c>SMOOTH_MODEL</c> (optional). Returns <c>null</c> when
    /// <c>SMOOTH_API_KEY</c> is unset — never a keyless client.
    /// </summary>
    public static ProviderRegistry? FromEnv()
    {
        var apiKey = Environment.GetEnvironmentVariable("SMOOTH_API_KEY");
        if (apiKey is null)
        {
            return null;
        }

        var providerId = Environment.GetEnvironmentVariable("SMOOTH_PROVIDER");
        if (string.IsNullOrEmpty(providerId))
        {
            providerId = "openrouter";
        }

        var config = providerId switch
        {
            "openai" => Providers.OpenAI(apiKey),
            "anthropic" => Providers.Anthropic(apiKey),
            "ollama" => Providers.Ollama() with { ApiKey = apiKey },
            "google" => Providers.Google(apiKey),
            "kimi" => Providers.Kimi(apiKey),
            "kimi-code" => Providers.KimiCode(apiKey),
            "llmgateway" => Providers.LlmGateway(apiKey),
            _ => Providers.OpenRouter(apiKey),
        };

        var model = Environment.GetEnvironmentVariable("SMOOTH_MODEL");
        if (string.IsNullOrEmpty(model))
        {
            model = config.DefaultModel;
        }

        var registry = new ProviderRegistry();
        registry.RegisterProvider(config);
        registry.Routing = ModelRouting.Uniform(new ModelSlot(providerId, model));
        return registry;
    }

    /// <summary>Deserialize a registry from the JSON shape <see cref="ToJson"/> writes.</summary>
    public static ProviderRegistry FromJson(string json)
    {
        var file = JsonSerializer.Deserialize<RegistryWire>(json)
                   ?? throw new InvalidOperationException("parsing provider registry JSON: empty document");

        var registry = new ProviderRegistry { Routing = FromWire(file.Routing) };
        foreach (var p in file.Providers ?? [])
        {
            registry.RegisterProvider(new ProviderConfig(p.Id, p.ApiUrl, p.ApiKey, Enum.Parse<ApiFormat>(p.ApiFormat), p.DefaultModel));
        }

        return registry;
    }

    /// <summary>Read a registry from a JSON file (e.g. <c>~/.smooth/providers.json</c>).</summary>
    public static ProviderRegistry LoadFromFile(string path) => FromJson(File.ReadAllText(path));

    /// <summary>Add (or replace) a provider configuration.</summary>
    public void RegisterProvider(ProviderConfig config) => _providers[config.Id] = config;

    /// <summary>Drop a provider by id.</summary>
    public void RemoveProvider(string id) => _providers.Remove(id);

    /// <summary>Look up a provider by id.</summary>
    public ProviderConfig? GetProvider(string id) => _providers.GetValueOrDefault(id);

    /// <summary>Every registered provider id, ordinal-sorted.</summary>
    public IReadOnlyList<string> ListProviders() => _providers.Keys.OrderBy(k => k, StringComparer.Ordinal).ToList();

    /// <summary>Point every routing slot at <paramref name="providerId"/> using its default model.</summary>
    public void SetDefaultProvider(string providerId) =>
        Routing = ModelRouting.Uniform(new ModelSlot(providerId, GetProvider(providerId)?.DefaultModel ?? string.Empty));

    private LlmConfig ResolveSlot(ModelSlot slot)
    {
        if (_providers.TryGetValue(slot.Provider, out var provider))
        {
            return new LlmConfig(provider.ApiUrl, provider.ApiKey, slot.Model, 32768, 0.0, provider.ApiFormat);
        }

        if (slot.Fallback is not null)
        {
            return ResolveSlot(slot.Fallback);
        }

        throw new InvalidOperationException($"provider '{slot.Provider}' not registered and no fallback available");
    }

    /// <summary>
    /// Resolve the route for an activity. Throws when the slot's provider — and every fallback — is
    /// unregistered, rather than silently substituting some other provider.
    /// </summary>
    public LlmConfig LlmConfigFor(Activity activity) => ResolveSlot(Routing.SlotFor(activity));

    /// <summary>Resolve the wire-compat <c>default</c> slot.</summary>
    public LlmConfig DefaultLlmConfig() => ResolveSlot(Routing.Default);

    /// <summary>
    /// Build a chat client for an activity's resolved route — the one line between "which model
    /// should this call use" and a client that speaks to it.
    ///
    /// <para>The client is OpenAI-compatible; an <see cref="ApiFormat.Anthropic"/> provider is
    /// rejected rather than silently spoken to in the wrong dialect.</para>
    /// </summary>
    public (GatewayChatClient Client, LlmConfig Config) ClientFor(Activity activity)
    {
        var config = LlmConfigFor(activity);
        if (config.ApiFormat != ApiFormat.OpenAiCompat)
        {
            throw new InvalidOperationException(
                $"activity {activity} routes to a {config.ApiFormat} provider, which the OpenAI-compatible gateway client cannot speak");
        }

        return (new GatewayChatClient(config.ApiUrl, config.ApiKey, config.Model), config);
    }

    /// <summary>Serialize to the on-disk JSON shape, snake_case keys and all.</summary>
    public string ToJson(bool pretty = false)
    {
        var file = new RegistryWire(
            ListProviders().Select(id => _providers[id]).Select(p =>
                new ProviderWire(p.Id, p.ApiUrl, p.ApiKey, p.ApiFormat.ToString(), p.DefaultModel)).ToList(),
            ToWire(Routing));
        return JsonSerializer.Serialize(file, new JsonSerializerOptions { WriteIndented = pretty });
    }

    /// <summary>Write the registry as pretty-printed JSON, creating parent directories.</summary>
    public void SaveToFile(string path)
    {
        var dir = Path.GetDirectoryName(path);
        if (!string.IsNullOrEmpty(dir))
        {
            Directory.CreateDirectory(dir);
        }

        File.WriteAllText(path, ToJson(pretty: true));
    }

    private static SlotWire ToWire(ModelSlot slot) =>
        new(slot.Provider, slot.Model, slot.Fallback is null ? null : ToWire(slot.Fallback));

    private static ModelSlot FromWire(SlotWire wire) =>
        new(wire.Provider, wire.Model, wire.Fallback is null ? null : FromWire(wire.Fallback));

    private static RoutingWire ToWire(ModelRouting r) => new(
        ToWire(r.Coding),
        r.Reasoning is null ? null : ToWire(r.Reasoning),
        // `thinking` is read-only compatibility: accepted on load, never written back.
        null,
        ToWire(r.Reviewing),
        ToWire(r.Judge),
        ToWire(r.Summarize),
        ToWire(r.Default),
        r.Fast is null ? null : ToWire(r.Fast),
        r.Planning is null ? null : ToWire(r.Planning));

    private static ModelRouting FromWire(RoutingWire w)
    {
        // An explicit `reasoning` always wins over the legacy `thinking` field name.
        var reasoning = w.Reasoning ?? w.Thinking;
        return new ModelRouting(
            FromWire(w.Coding),
            FromWire(w.Reviewing),
            FromWire(w.Judge),
            FromWire(w.Summarize),
            FromWire(w.Default),
            reasoning is null ? null : FromWire(reasoning),
            w.Fast is null ? null : FromWire(w.Fast),
            w.Planning is null ? null : FromWire(w.Planning));
    }
}
