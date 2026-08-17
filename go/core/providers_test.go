package core

// Provider-routing parity tests — the Go half of the cross-language contract.
//
// TestRoutingMatchesSharedCorpus is the drift gate: it replays
// spec/providers/routing.json (generated FROM the Rust reference) and asserts
// this port resolves every preset slot to the same model, base URL, key and wire
// format, matches the same quirks, builds the same /model/info URLs, and parses
// the same alias maps. The rest port the Rust engine's own unit tests —
// fallback chains, on-disk wire compatibility, env loading, save/load round-trip.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type corpusSlot struct {
	Model       string  `json:"model"`
	APIURL      string  `json:"apiUrl"`
	APIKey      string  `json:"apiKey"`
	APIFormat   string  `json:"apiFormat"`
	MaxTokens   int     `json:"maxTokens"`
	Temperature float64 `json:"temperature"`
}

type routingCorpus struct {
	PresetNames []struct {
		Name   string  `json:"name"`
		Preset *string `json:"preset"`
	} `json:"presetNames"`
	DefaultRouting map[string]struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	} `json:"defaultRouting"`
	WireCompat []struct {
		ID         string            `json:"id"`
		JSON       string            `json:"json"`
		SlotModels map[string]string `json:"slotModels"`
	} `json:"wireCompat"`
	FallbackChain struct {
		APIURL string `json:"apiUrl"`
		Model  string `json:"model"`
		APIKey string `json:"apiKey"`
	} `json:"fallbackChain"`
	UnregisteredWithoutFallbackErrors bool `json:"unregisteredWithoutFallbackErrors"`
	Presets                           []struct {
		Name                string                `json:"name"`
		ProviderID          string                `json:"providerId"`
		RegisteredProviders []string              `json:"registeredProviders"`
		Slots               map[string]corpusSlot `json:"slots"`
	} `json:"presets"`
	ProviderFactories []struct {
		Factory      string `json:"factory"`
		ID           string `json:"id"`
		APIURL       string `json:"apiUrl"`
		APIKey       string `json:"apiKey"`
		APIFormat    string `json:"apiFormat"`
		DefaultModel string `json:"defaultModel"`
	} `json:"providerFactories"`
	Quirks []struct {
		Upstream           string   `json:"upstream"`
		StrictToolCallJSON bool     `json:"strictToolCallJson"`
		AllowParallelTools *bool    `json:"allowParallelTools"`
		MatchedKeys        []string `json:"matchedKeys"`
	} `json:"quirks"`
	ModelInfoURLs []struct {
		APIURL       string `json:"apiUrl"`
		ModelInfoURL string `json:"modelInfoUrl"`
	} `json:"modelInfoUrls"`
	ModelInfoParse []struct {
		ID      string `json:"id"`
		Body    string `json:"body"`
		Entries []struct {
			Alias    string  `json:"alias"`
			Upstream *string `json:"upstream"`
			ID       *string `json:"id"`
		} `json:"entries"`
	} `json:"modelInfoParse"`
}

func loadRoutingCorpus(t *testing.T) routingCorpus {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "spec", "providers", "routing.json"))
	if err != nil {
		t.Fatalf("resolve corpus path: %v", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	var c routingCorpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	if len(c.Presets) != 5 {
		t.Fatalf("corpus must carry all 5 presets, got %d", len(c.Presets))
	}
	return c
}

// clearGatewayURL removes the SMOOAI_GATEWAY_URL override for the duration of a
// test — the corpus pins the production default, which only applies when the
// variable is ABSENT.
func clearGatewayURL(t *testing.T) {
	t.Helper()
	prior, had := os.LookupEnv("SMOOAI_GATEWAY_URL")
	if err := os.Unsetenv("SMOOAI_GATEWAY_URL"); err != nil {
		t.Fatalf("unset SMOOAI_GATEWAY_URL: %v", err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("SMOOAI_GATEWAY_URL", prior)
		} else {
			_ = os.Unsetenv("SMOOAI_GATEWAY_URL")
		}
	})
}

func TestRoutingMatchesSharedCorpus(t *testing.T) {
	clearGatewayURL(t)
	corpus := loadRoutingCorpus(t)

	activities := map[string]Activity{
		"coding": ActivityCoding, "reasoning": ActivityReasoning, "reviewing": ActivityReviewing,
		"judge": ActivityJudge, "summarize": ActivitySummarize, "fast": ActivityFast,
	}

	t.Run("presets", func(t *testing.T) {
		for _, p := range corpus.Presets {
			t.Run(p.Name, func(t *testing.T) {
				preset, ok := PresetFromName(p.Name)
				if !ok {
					t.Fatalf("PresetFromName(%q) did not resolve", p.Name)
				}
				if got := preset.ProviderID(); got != p.ProviderID {
					t.Errorf("provider id: got %q want %q", got, p.ProviderID)
				}
				registry := RegistryFromPreset(preset, "test-key")
				got := registry.ListProviders()
				if len(got) != len(p.RegisteredProviders) {
					t.Fatalf("registered providers: got %v want %v", got, p.RegisteredProviders)
				}
				for i, id := range p.RegisteredProviders {
					if got[i] != id {
						t.Errorf("registered providers: got %v want %v", got, p.RegisteredProviders)
					}
				}
				for label, want := range p.Slots {
					var config LlmConfig
					var err error
					if label == "default" {
						config, err = registry.DefaultLlmConfig()
					} else {
						config, err = registry.LlmConfigFor(activities[label])
					}
					if err != nil {
						t.Fatalf("%s slot did not resolve: %v", label, err)
					}
					if config.Model != want.Model || config.APIURL != want.APIURL || config.APIKey != want.APIKey ||
						string(config.APIFormat) != want.APIFormat || config.MaxTokens != want.MaxTokens || config.Temperature != want.Temperature {
						t.Errorf("%s slot mismatch\n got: %+v\nwant: %+v", label, config, want)
					}
				}
			})
		}
	})

	t.Run("preset_names", func(t *testing.T) {
		for _, v := range corpus.PresetNames {
			preset, ok := PresetFromName(v.Name)
			if v.Preset == nil {
				if ok {
					t.Errorf("PresetFromName(%q) should not resolve, got %v", v.Name, preset)
				}
				continue
			}
			if !ok {
				t.Errorf("PresetFromName(%q) should resolve to provider %q", v.Name, *v.Preset)
			} else if preset.ProviderID() != *v.Preset {
				t.Errorf("PresetFromName(%q) provider: got %q want %q", v.Name, preset.ProviderID(), *v.Preset)
			}
		}
	})

	t.Run("provider_factories", func(t *testing.T) {
		factories := map[string]ProviderConfig{
			"openrouter":    OpenRouterProvider("k"),
			"openai":        OpenAIProvider("k"),
			"anthropic":     AnthropicProvider("k"),
			"ollama":        OllamaProvider(),
			"google":        GoogleProvider("k"),
			"kimi":          KimiProvider("k"),
			"kimiCode":      KimiCodeProvider("k"),
			"llmgateway":    LlmGatewayProvider("k"),
			"smooaiGateway": SmooaiGatewayProvider("k"),
		}
		for _, want := range corpus.ProviderFactories {
			got, ok := factories[want.Factory]
			if !ok {
				t.Fatalf("no Go factory for %q", want.Factory)
			}
			if got.ID != want.ID || got.APIURL != want.APIURL || got.APIKey != want.APIKey ||
				string(got.APIFormat) != want.APIFormat || got.DefaultModel != want.DefaultModel {
				t.Errorf("%s factory mismatch\n got: %+v\nwant: %+v", want.Factory, got, want)
			}
		}
	})

	t.Run("default_routing", func(t *testing.T) {
		routing := DefaultModelRouting()
		for label, want := range corpus.DefaultRouting {
			var slot *ModelSlot
			if label == "default" {
				slot = &routing.Default
			} else {
				slot = routing.SlotFor(activities[label])
			}
			if slot.Provider != want.Provider || slot.Model != want.Model {
				t.Errorf("%s: got %s/%s want %s/%s", label, slot.Provider, slot.Model, want.Provider, want.Model)
			}
		}
		// The hosted gateway is opt-in, never the default.
		if routing.Coding.Provider == "smooai-gateway" {
			t.Error("the Smoo AI gateway must never be the default provider")
		}
	})

	t.Run("wire_compat", func(t *testing.T) {
		for _, v := range corpus.WireCompat {
			registry, err := RegistryFromJSON(v.JSON)
			if err != nil {
				t.Fatalf("%s did not parse: %v", v.ID, err)
			}
			for label, want := range v.SlotModels {
				var got string
				if label == "default" {
					got = registry.Routing.Default.Model
				} else {
					got = registry.Routing.SlotFor(activities[label]).Model
				}
				if got != want {
					t.Errorf("%s/%s: got %q want %q", v.ID, label, got, want)
				}
			}
		}
	})

	t.Run("fallback_chain", func(t *testing.T) {
		registry := NewProviderRegistry()
		registry.RegisterProvider(ProviderConfig{"tertiary", "https://tertiary.example.com/v1", "t-key", APIFormatOpenAICompat, "model-c"})
		registry.Routing.Coding = NewModelSlot("primary", "model-a").
			WithFallback(NewModelSlot("secondary", "model-b").WithFallback(NewModelSlot("tertiary", "model-c")))

		config, err := registry.LlmConfigFor(ActivityCoding)
		if err != nil {
			t.Fatalf("chain should resolve: %v", err)
		}
		want := corpus.FallbackChain
		if config.APIURL != want.APIURL || config.Model != want.Model || config.APIKey != want.APIKey {
			t.Errorf("chain resolved to %+v, want %+v", config, want)
		}
	})

	t.Run("unregistered_without_fallback_errors", func(t *testing.T) {
		registry := NewProviderRegistry()
		registry.Routing.Coding = NewModelSlot("nope", "m")
		_, err := registry.LlmConfigFor(ActivityCoding)
		if (err != nil) != corpus.UnregisteredWithoutFallbackErrors {
			t.Errorf("unregistered provider with no fallback: err=%v, corpus expects error=%v", err, corpus.UnregisteredWithoutFallbackErrors)
		}
	})

	t.Run("quirks", func(t *testing.T) {
		for _, v := range corpus.Quirks {
			got := QuirksForModel(v.Upstream)
			if got.StrictToolCallJSON != v.StrictToolCallJSON {
				t.Errorf("%q strictToolCallJson: got %v want %v", v.Upstream, got.StrictToolCallJSON, v.StrictToolCallJSON)
			}
			if (got.AllowParallelTools == nil) != (v.AllowParallelTools == nil) {
				t.Errorf("%q allowParallelTools presence mismatch", v.Upstream)
			}
			snapshot := QuirksDebugSnapshot(v.Upstream)
			if len(snapshot) != len(v.MatchedKeys) {
				t.Errorf("%q matched keys: got %v want %v", v.Upstream, snapshot, v.MatchedKeys)
			}
			for _, key := range v.MatchedKeys {
				if _, ok := snapshot[key]; !ok {
					t.Errorf("%q should match quirk key %q", v.Upstream, key)
				}
			}
		}
	})

	t.Run("model_info_urls", func(t *testing.T) {
		for _, v := range corpus.ModelInfoURLs {
			if got := BuildModelInfoURL(v.APIURL); got != v.ModelInfoURL {
				t.Errorf("BuildModelInfoURL(%q): got %q want %q", v.APIURL, got, v.ModelInfoURL)
			}
		}
	})

	t.Run("model_info_parse", func(t *testing.T) {
		for _, v := range corpus.ModelInfoParse {
			got, err := ParseModelInfo(v.Body)
			if err != nil {
				t.Fatalf("%s did not parse: %v", v.ID, err)
			}
			aliases := SortedAliases(got)
			if len(aliases) != len(v.Entries) {
				t.Fatalf("%s: got %v want %d entries", v.ID, aliases, len(v.Entries))
			}
			// The corpus lists entries alias-sorted (Rust returns a BTreeMap).
			for i, want := range v.Entries {
				if aliases[i] != want.Alias {
					t.Errorf("%s: alias order got %v want index %d = %q", v.ID, aliases, i, want.Alias)
					continue
				}
				entry := got[want.Alias]
				wantUpstream, wantID := "", ""
				if want.Upstream != nil {
					wantUpstream = *want.Upstream
				}
				if want.ID != nil {
					wantID = *want.ID
				}
				if entry.Upstream != wantUpstream || entry.ID != wantID {
					t.Errorf("%s/%s: got upstream=%q id=%q want upstream=%q id=%q", v.ID, want.Alias, entry.Upstream, entry.ID, wantUpstream, wantID)
				}
			}
		}
	})
}

func TestParseModelInfoRejectsInvalidJSON(t *testing.T) {
	if _, err := ParseModelInfo("not json"); err == nil {
		t.Fatal("invalid JSON must be an error, not an empty map")
	}
}

// TestRegistryRoundTripsOnDiskShape pins the providers.json field names. The
// same file is written by the Rust CLI and read here, so a renamed key is a
// silent config loss, not a compile error.
func TestRegistryRoundTripsOnDiskShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "providers.json")

	registry := NewProviderRegistry()
	registry.RegisterProvider(OpenRouterProvider("or-key"))
	registry.RegisterProvider(OpenAIProvider("oai-key"))
	if err := registry.SaveToFile(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var shape struct {
		Providers []map[string]any `json:"providers"`
		Routing   map[string]any   `json:"routing"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("parse written file: %v", err)
	}
	for _, key := range []string{"id", "api_url", "api_key", "api_format", "default_model"} {
		if _, ok := shape.Providers[0][key]; !ok {
			t.Errorf("provider entry is missing the on-disk key %q — Rust will not read this file", key)
		}
	}
	for _, key := range []string{"coding", "reasoning", "reviewing", "judge", "summarize", "default", "fast"} {
		if _, ok := shape.Routing[key]; !ok {
			t.Errorf("routing is missing the on-disk key %q", key)
		}
	}
	// `planning` is legacy: accepted on read, never written by a fresh config.
	if _, ok := shape.Routing["planning"]; ok {
		t.Error("a fresh config must not write the legacy `planning` slot")
	}

	loaded, err := LoadRegistryFromFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := loaded.ListProviders(); len(got) != 2 {
		t.Fatalf("providers survived: %v", got)
	}
	or, _ := loaded.GetProvider("openrouter")
	if or.APIKey != "or-key" {
		t.Errorf("openrouter key: %q", or.APIKey)
	}
	config, err := loaded.LlmConfigFor(ActivityReasoning)
	if err != nil {
		t.Fatalf("routing survived roundtrip: %v", err)
	}
	if config.Model != "openrouter/auto" || config.APIKey != "or-key" {
		t.Errorf("roundtripped routing: %+v", config)
	}
}

// TestModelSlotOmitsFallbackWhenAbsent guards the `omitempty` — Rust skips the
// field entirely and a written `"fallback": null` is not the same document.
func TestModelSlotOmitsFallbackWhenAbsent(t *testing.T) {
	data, err := json.Marshal(NewModelSlot("openrouter", "openai/gpt-4o"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(data); got != `{"provider":"openrouter","model":"openai/gpt-4o"}` {
		t.Errorf("slot JSON: %s", got)
	}
}

func TestRegistryFromEnv(t *testing.T) {
	t.Setenv("SMOOTH_API_KEY", "env-test-key")
	t.Setenv("SMOOTH_PROVIDER", "openai")
	if err := os.Unsetenv("SMOOTH_MODEL"); err != nil {
		t.Fatalf("unset SMOOTH_MODEL: %v", err)
	}

	registry, ok := RegistryFromEnv()
	if !ok {
		t.Fatal("should load from env")
	}
	provider, ok := registry.GetProvider("openai")
	if !ok || provider.APIKey != "env-test-key" {
		t.Fatalf("provider: %+v", provider)
	}
	config, err := registry.DefaultLlmConfig()
	if err != nil {
		t.Fatalf("default config: %v", err)
	}
	if config.Model != "gpt-4o" {
		t.Errorf("model should default to the provider's default model, got %q", config.Model)
	}

	// SMOOTH_MODEL overrides the provider's default.
	t.Setenv("SMOOTH_MODEL", "gpt-4o-mini")
	registry, _ = RegistryFromEnv()
	config, _ = registry.DefaultLlmConfig()
	if config.Model != "gpt-4o-mini" {
		t.Errorf("SMOOTH_MODEL should win, got %q", config.Model)
	}
}

func TestRegistryFromEnvNeedsAKey(t *testing.T) {
	if err := os.Unsetenv("SMOOTH_API_KEY"); err != nil {
		t.Fatalf("unset: %v", err)
	}
	if _, ok := RegistryFromEnv(); ok {
		t.Error("no SMOOTH_API_KEY means no registry — never a keyless client")
	}
}

func TestSmooaiGatewayRespectsURLOverride(t *testing.T) {
	t.Setenv("SMOOAI_GATEWAY_URL", "https://llm.dev.smooai.com/v1")
	registry := RegistryFromPreset(PresetSmooaiGateway, "dev-key")
	config, err := registry.DefaultLlmConfig()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if config.APIURL != "https://llm.dev.smooai.com/v1" || config.APIKey != "dev-key" {
		t.Errorf("override ignored: %+v", config)
	}
}

func TestSetDefaultProviderAndRemove(t *testing.T) {
	registry := NewProviderRegistry()
	registry.RegisterProvider(KimiProvider("k-key"))
	registry.SetDefaultProvider("kimi")

	for _, activity := range []Activity{ActivityCoding, ActivityReasoning, ActivityReviewing, ActivityJudge, ActivitySummarize, ActivityFast} {
		config, err := registry.LlmConfigFor(activity)
		if err != nil {
			t.Fatalf("%s: %v", activity, err)
		}
		if config.Model != "kimi-k2.5" || config.APIURL != "https://api.moonshot.ai/v1" {
			t.Errorf("%s routed to %+v", activity, config)
		}
	}

	registry.RemoveProvider("kimi")
	if _, err := registry.LlmConfigFor(ActivityCoding); err == nil {
		t.Error("removing the provider must break resolution, not silently fall through")
	}
}

// TestClientForRejectsNonOpenAIFormat is the integration point: a resolved route
// becomes a live client. An Anthropic-dialect provider must be refused rather
// than spoken to in OpenAI's wire format.
func TestClientForRejectsNonOpenAIFormat(t *testing.T) {
	openAICompat := RegistryFromPreset(PresetOpenAI, "k")
	client, config, err := openAICompat.ClientFor(ActivityCoding)
	if err != nil {
		t.Fatalf("OpenAI-compatible route should build a client: %v", err)
	}
	if client == nil {
		t.Fatal("client should not be nil")
	}
	if config.Model != "gpt-4o" {
		t.Errorf("resolved config should accompany the client, got %+v", config)
	}

	anthropic := RegistryFromPreset(PresetAnthropic, "k")
	if _, _, err := anthropic.ClientFor(ActivityCoding); err == nil {
		t.Error("an Anthropic-format provider must not be handed to the OpenAI-compatible client")
	}
}

func TestProviderConfigStringRedactsTheKey(t *testing.T) {
	got := OpenRouterProvider("super-secret-key").String()
	if got == "" || strings.Contains(got, "super-secret-key") {
		t.Errorf("provider String must not leak the key: %s", got)
	}
	config := LlmConfig{APIURL: "u", APIKey: "super-secret-key", Model: "m"}
	if strings.Contains(config.String(), "super-secret-key") {
		t.Errorf("LlmConfig String must not leak the key: %s", config.String())
	}
}
