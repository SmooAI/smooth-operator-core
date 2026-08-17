package core

// Provider routing — the Go port of the Rust reference engine's `providers.rs`,
// `quirks.rs` and `resolution.rs`.
//
// Three concerns, one module because they are one story: WHICH model a given
// activity should use, WHAT wire quirks that concrete model has, and — when the
// route points at a LiteLLM-style gateway — WHICH upstream model a semantic
// alias actually resolves to.
//
//   - [ProviderRegistry] holds provider credentials/URLs and a [ModelRouting]
//     table mapping each [Activity] to a [ModelSlot]. LlmConfigFor walks the
//     slot's fallback chain until it finds a registered provider.
//   - [QuirksForModel] looks up per-model wire quirks by substring on the
//     concrete upstream name.
//   - [BuildModelInfoURL] / [ParseModelInfo] / [FetchModelInfo] recover the
//     gateway's alias → upstream map from `GET /model/info`.
//
// The on-disk JSON shape is shared with the Rust CLI (`~/.smooth/providers.json`),
// so the struct tags below are snake_case and must stay byte-compatible: the same
// file is written by one engine and read by another. Legacy `thinking` /
// `planning` field names still deserialize onto the merged `reasoning` slot.
//
// Routing values are pinned across all five engines by the shared corpus at
// spec/providers/routing.json (see providers_test.go) — a slot that resolves to
// the wrong model or base URL sends real traffic and real money somewhere nobody
// intended, and it looks like it is working.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Providers
// ---------------------------------------------------------------------------

// APIFormat is the wire dialect a provider speaks. The JSON encoding matches the
// Rust reference's serde output so `providers.json` round-trips between engines.
type APIFormat string

const (
	// APIFormatOpenAICompat is the OpenAI `/chat/completions` dialect.
	APIFormatOpenAICompat APIFormat = "OpenAiCompat"
	// APIFormatAnthropic is Anthropic's native `/messages` dialect.
	APIFormatAnthropic APIFormat = "Anthropic"
)

// ProviderConfig is the connection detail for a single LLM provider.
type ProviderConfig struct {
	ID           string    `json:"id"`
	APIURL       string    `json:"api_url"`
	APIKey       string    `json:"api_key"`
	APIFormat    APIFormat `json:"api_format"`
	DefaultModel string    `json:"default_model"`
}

// String redacts the API key so it never lands in logs or error chains.
// Everything else is printed verbatim. Mirrors the Rust reference's manual
// Debug impl.
func (p ProviderConfig) String() string {
	return fmt.Sprintf("ProviderConfig{id:%s api_url:%s api_key:***redacted*** api_format:%s default_model:%s}",
		p.ID, p.APIURL, p.APIFormat, p.DefaultModel)
}

// OpenRouterProvider is OpenRouter — an OpenAI-compatible proxy for many models.
func OpenRouterProvider(apiKey string) ProviderConfig {
	return ProviderConfig{"openrouter", "https://openrouter.ai/api/v1", apiKey, APIFormatOpenAICompat, "openai/gpt-4o"}
}

// OpenAIProvider is the OpenAI direct API.
func OpenAIProvider(apiKey string) ProviderConfig {
	return ProviderConfig{"openai", "https://api.openai.com/v1", apiKey, APIFormatOpenAICompat, "gpt-4o"}
}

// AnthropicProvider is the Anthropic native API.
func AnthropicProvider(apiKey string) ProviderConfig {
	return ProviderConfig{"anthropic", "https://api.anthropic.com/v1", apiKey, APIFormatAnthropic, "claude-sonnet-4-20250514"}
}

// OllamaProvider is a local Ollama instance — no API key needed.
func OllamaProvider() ProviderConfig {
	return ProviderConfig{"ollama", "http://localhost:11434/v1", "", APIFormatOpenAICompat, "llama3"}
}

// GoogleProvider is the Google Gemini API (OpenAI-compatible surface).
func GoogleProvider(apiKey string) ProviderConfig {
	return ProviderConfig{"google", "https://generativelanguage.googleapis.com/v1beta/openai", apiKey, APIFormatOpenAICompat, "gemini-2.0-flash"}
}

// KimiProvider is Moonshot AI's general-purpose API (OpenAI-compatible).
func KimiProvider(apiKey string) ProviderConfig {
	return ProviderConfig{"kimi", "https://api.moonshot.ai/v1", apiKey, APIFormatOpenAICompat, "kimi-k2.5"}
}

// KimiCodeProvider is Moonshot's coding-optimized API (Anthropic-compatible).
func KimiCodeProvider(apiKey string) ProviderConfig {
	return ProviderConfig{"kimi-code", "https://api.kimi.com/coding/v1", apiKey, APIFormatAnthropic, "kimi-for-coding"}
}

// LlmGatewayProvider is LLM Gateway — a unified API for 210+ models.
func LlmGatewayProvider(apiKey string) ProviderConfig {
	return ProviderConfig{"llmgateway", "https://api.llmgateway.io/v1", apiKey, APIFormatOpenAICompat, "openai/gpt-4o"}
}

// SmooaiGatewayProvider is the hosted LiteLLM-backed gateway run by Smoo AI.
//
// One API key, one URL, OpenAI-compatible. The gateway handles provider
// selection, billing, moderation and cost tracking server-side, so consumers
// reference models by semantic aliases (`smooth-coding`, `smooth-judge`, …) that
// the gateway maps to whichever underlying model is currently best — upgrades
// ship server-side with no client release.
//
// SMOOAI_GATEWAY_URL overrides the base URL for self-hosted installs or dev.
func SmooaiGatewayProvider(apiKey string) ProviderConfig {
	// LookupEnv, not Getenv: a set-but-empty override yields an empty base URL,
	// matching the Rust reference. Only an ABSENT variable takes the default.
	apiURL, ok := os.LookupEnv("SMOOAI_GATEWAY_URL")
	if !ok {
		apiURL = "https://llm.smoo.ai/v1"
	}
	return ProviderConfig{"smooai-gateway", apiURL, apiKey, APIFormatOpenAICompat, "smooth-default"}
}

// ---------------------------------------------------------------------------
// Presets
// ---------------------------------------------------------------------------

// Preset is a ready-made provider + routing configuration.
type Preset string

const (
	// PresetSmooaiGateway is the hosted Smoo AI gateway — the recommended default.
	PresetSmooaiGateway Preset = "SmoaiGateway"
	// PresetOpenRouterLowCost routes Chinese frontier models via OpenRouter — cheapest.
	PresetOpenRouterLowCost Preset = "OpenRouterLowCost"
	// PresetLlmGatewayLowCost routes Chinese frontier models via LLM Gateway.
	PresetLlmGatewayLowCost Preset = "LlmGatewayLowCost"
	// PresetOpenAI routes OpenAI models.
	PresetOpenAI Preset = "OpenAI"
	// PresetAnthropic routes Anthropic Claude models.
	PresetAnthropic Preset = "Anthropic"
)

// PresetInfo is one row of [AllPresets]: CLI name, display label, description.
type PresetInfo struct {
	Name        string
	Label       string
	Description string
}

// AllPresets lists every preset. The first entry is the recommended default —
// `th auth login` shows them in this order.
var AllPresets = []PresetInfo{
	{"smooai-gateway", "Smoo AI Gateway (recommended)", "Hosted LiteLLM gateway run by Smoo AI — billing, moderation, governance, 100+ models. One key, one URL, no config."},
	{"openrouter-low-cost", "OpenRouter Low Cost", "GLM-5.1 thinking (#1 SWE-Bench Pro), MiniMax-M2.7 coding (56% SWE-Pro, 10B params), DeepSeek-V3.2 default"},
	{"llmgateway-low-cost", "LLM Gateway Low Cost", "GLM-5 thinking, MiniMax-M2.7 coding, DeepSeek-V3.2 default — unified billing, 224 models"},
	{"openai", "OpenAI", "o3-mini thinking, GPT-4o coding — OpenAI ecosystem"},
	{"anthropic", "Anthropic", "Claude Opus thinking, Sonnet coding — highest quality"},
}

// PresetFromName parses a preset name or alias. Returns false for unknown names.
func PresetFromName(name string) (Preset, bool) {
	switch name {
	case "smooai-gateway", "smooai", "gateway":
		return PresetSmooaiGateway, true
	case "openrouter-low-cost", "low-cost":
		return PresetOpenRouterLowCost, true
	case "llmgateway-low-cost", "gateway-low-cost":
		return PresetLlmGatewayLowCost, true
	case "openai", "codex":
		return PresetOpenAI, true
	case "anthropic":
		return PresetAnthropic, true
	default:
		return "", false
	}
}

// ProviderID is the provider this preset requires.
func (p Preset) ProviderID() string {
	switch p {
	case PresetSmooaiGateway:
		return "smooai-gateway"
	case PresetOpenRouterLowCost:
		return "openrouter"
	case PresetLlmGatewayLowCost:
		return "llmgateway"
	case PresetOpenAI:
		return "openai"
	case PresetAnthropic:
		return "anthropic"
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

// Activity selects which model slot a call routes through. Six semantic slots:
// the legacy Thinking + Planning split collapsed into Reasoning, and the legacy
// "default" alias is served by Coding.
type Activity string

const (
	// ActivityCoding is the outer coding loop — the workhorse slot, which also
	// serves the legacy "default" call path.
	ActivityCoding Activity = "Coding"
	// ActivityReasoning is deep reasoning / planning / chain-of-thought.
	ActivityReasoning Activity = "Reasoning"
	// ActivityReviewing is code review, critique, adversarial checks.
	ActivityReviewing Activity = "Reviewing"
	// ActivityJudge is LLM-as-a-judge: yes/no verdicts, low latency, used by
	// Narc guardrails and bench scoring.
	ActivityJudge Activity = "Judge"
	// ActivitySummarize is context compression during long agent runs.
	ActivitySummarize Activity = "Summarize"
	// ActivityFast is small, latency-sensitive utility calls: session
	// auto-naming, short titles, one-liner summaries, autocomplete. Sub-second
	// first token, short output, no tool use — don't pay Sonnet-plus prices to
	// name a session.
	ActivityFast Activity = "Fast"
)

// ModelSlot binds a provider ID and model name, with an optional fallback used
// when the provider is not registered.
type ModelSlot struct {
	Provider string     `json:"provider"`
	Model    string     `json:"model"`
	Fallback *ModelSlot `json:"fallback,omitempty"`
}

// NewModelSlot builds a slot with no fallback.
func NewModelSlot(provider, model string) ModelSlot {
	return ModelSlot{Provider: provider, Model: model}
}

// WithFallback returns a copy of the slot with fallback attached.
func (s ModelSlot) WithFallback(fallback ModelSlot) ModelSlot {
	s.Fallback = &fallback
	return s
}

// ModelRouting is the per-activity routing table.
//
// Six semantic slots plus a Default slot kept for wire compatibility: no
// Activity routes through Default directly (ActivityCoding serves the default
// path), but the field stays so pre-collapse configs load cleanly.
type ModelRouting struct {
	Coding ModelSlot `json:"coding"`
	// Reasoning is the merged deep-reasoning slot. Optional on disk: older
	// files carry `thinking` instead, and SlotFor falls back to Default when
	// it is absent entirely.
	Reasoning *ModelSlot `json:"reasoning,omitempty"`
	Reviewing ModelSlot  `json:"reviewing"`
	Judge     ModelSlot  `json:"judge"`
	Summarize ModelSlot  `json:"summarize"`
	Default   ModelSlot  `json:"default"`
	// Fast is optional on disk: pre-fast providers.json files deserialize with
	// Fast nil and the router falls back to Default.
	Fast *ModelSlot `json:"fast,omitempty"`
	// Planning is a legacy field held for wire compatibility. Deserialized but
	// ignored at lookup time — Reasoning absorbed the planning slot.
	Planning *ModelSlot `json:"planning,omitempty"`
}

// routingWire mirrors ModelRouting plus the legacy `thinking` field name, which
// Go's encoding/json cannot express as an alias the way serde can.
type routingWire struct {
	Coding    ModelSlot  `json:"coding"`
	Reasoning *ModelSlot `json:"reasoning,omitempty"`
	Thinking  *ModelSlot `json:"thinking,omitempty"`
	Reviewing ModelSlot  `json:"reviewing"`
	Judge     ModelSlot  `json:"judge"`
	Summarize ModelSlot  `json:"summarize"`
	Default   ModelSlot  `json:"default"`
	Fast      *ModelSlot `json:"fast,omitempty"`
	Planning  *ModelSlot `json:"planning,omitempty"`
}

// UnmarshalJSON migrates a legacy `thinking` slot onto Reasoning. Rust does this
// with `#[serde(alias = "thinking")]`; an explicit shadow struct is the Go
// equivalent. An explicit `reasoning` always wins over `thinking`.
func (r *ModelRouting) UnmarshalJSON(data []byte) error {
	var w routingWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	reasoning := w.Reasoning
	if reasoning == nil {
		reasoning = w.Thinking
	}
	*r = ModelRouting{
		Coding:    w.Coding,
		Reasoning: reasoning,
		Reviewing: w.Reviewing,
		Judge:     w.Judge,
		Summarize: w.Summarize,
		Default:   w.Default,
		Fast:      w.Fast,
		Planning:  w.Planning,
	}
	return nil
}

// DefaultModelRouting is the neutral, provider-agnostic routing every slot
// starts on: the well-known `openrouter` provider id with a placeholder `auto`
// model, so the library ships no opinion about a specific hosted gateway.
// Consumers opt into the Smoo AI gateway via [PresetSmooaiGateway] explicitly.
func DefaultModelRouting() ModelRouting {
	slot := NewModelSlot("openrouter", "openrouter/auto")
	reasoning, fast := slot, slot
	return ModelRouting{
		Coding:    slot,
		Reasoning: &reasoning,
		Reviewing: slot,
		Judge:     slot,
		Summarize: slot,
		Default:   slot,
		Fast:      &fast,
	}
}

// SlotFor returns the slot for an activity. Reasoning and Fast fall back to
// Default when absent so partial configs stay functional.
func (r *ModelRouting) SlotFor(activity Activity) *ModelSlot {
	switch activity {
	case ActivityCoding:
		return &r.Coding
	case ActivityReasoning:
		if r.Reasoning != nil {
			return r.Reasoning
		}
		return &r.Default
	case ActivityReviewing:
		return &r.Reviewing
	case ActivityJudge:
		return &r.Judge
	case ActivitySummarize:
		return &r.Summarize
	case ActivityFast:
		if r.Fast != nil {
			return r.Fast
		}
		return &r.Default
	default:
		return &r.Default
	}
}

// uniformRouting points every slot at the same target.
func uniformRouting(slot ModelSlot) ModelRouting {
	reasoning, fast := slot, slot
	return ModelRouting{
		Coding:    slot,
		Reasoning: &reasoning,
		Reviewing: slot,
		Judge:     slot,
		Summarize: slot,
		Default:   slot,
		Fast:      &fast,
	}
}

// ---------------------------------------------------------------------------
// Resolved config
// ---------------------------------------------------------------------------

// LlmConfig is a fully resolved route: the provider connection plus the model
// the activity picked. Feed APIURL/APIKey/Model to [NewGatewayClient].
type LlmConfig struct {
	APIURL      string
	APIKey      string
	Model       string
	MaxTokens   int
	Temperature float64
	APIFormat   APIFormat
}

// String redacts the API key, same as [ProviderConfig.String].
func (c LlmConfig) String() string {
	return fmt.Sprintf("LlmConfig{api_url:%s api_key:***redacted*** model:%s max_tokens:%d temperature:%v api_format:%s}",
		c.APIURL, c.Model, c.MaxTokens, c.Temperature, c.APIFormat)
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// registryFile is the on-disk JSON shape, shared byte-for-byte with the Rust CLI.
type registryFile struct {
	Providers []ProviderConfig `json:"providers"`
	Routing   ModelRouting     `json:"routing"`
}

// ProviderRegistry holds registered providers and the per-activity routing table.
type ProviderRegistry struct {
	providers map[string]ProviderConfig
	// Routing is the per-activity table. Mutate it directly to re-point a slot.
	Routing ModelRouting
}

// NewProviderRegistry creates an empty registry with the neutral default routing.
func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{providers: map[string]ProviderConfig{}, Routing: DefaultModelRouting()}
}

// RegistryFromPreset creates a registry pre-configured with a preset: it
// registers the preset's provider and installs routing tuned for the preset's
// goals (cost, quality, latency).
func RegistryFromPreset(preset Preset, apiKey string) *ProviderRegistry {
	r := NewProviderRegistry()
	slot := NewModelSlot
	ptr := func(s ModelSlot) *ModelSlot { return &s }

	switch preset {
	case PresetSmooaiGateway:
		// Semantic aliases the gateway's LiteLLM config maps to whichever
		// underlying model is currently best. Changing the underlying model is
		// a server-side deploy — no client release needed.
		r.RegisterProvider(SmooaiGatewayProvider(apiKey))
		r.Routing = ModelRouting{
			Coding:    slot("smooai-gateway", "smooth-coding"),
			Reasoning: ptr(slot("smooai-gateway", "smooth-reasoning")),
			Reviewing: slot("smooai-gateway", "smooth-reviewing"),
			Judge:     slot("smooai-gateway", "smooth-judge"),
			Summarize: slot("smooai-gateway", "smooth-summarize"),
			Default:   slot("smooai-gateway", "smooth-default"),
			Fast:      ptr(slot("smooai-gateway", "smooth-fast")),
		}
	case PresetOpenRouterLowCost:
		// OpenRouter uses provider-prefixed model IDs.
		r.RegisterProvider(OpenRouterProvider(apiKey))
		r.Routing = ModelRouting{
			Coding:    slot("openrouter", "minimax/minimax-m2.7").WithFallback(slot("openrouter", "minimax/minimax-m2.5")),
			Reasoning: ptr(slot("openrouter", "z-ai/glm-5.1")),
			Reviewing: slot("openrouter", "deepseek/deepseek-v3.2"),
			Judge:     slot("openrouter", "google/gemini-2.5-flash"),
			Summarize: slot("openrouter", "deepseek/deepseek-v3.2"),
			Default:   slot("openrouter", "deepseek/deepseek-v3.2"),
			Fast:      ptr(slot("openrouter", "google/gemini-2.5-flash-lite")),
		}
	case PresetLlmGatewayLowCost:
		// LLM Gateway uses bare model names.
		r.RegisterProvider(LlmGatewayProvider(apiKey))
		r.Routing = ModelRouting{
			Coding:    slot("llmgateway", "minimax-m2.7").WithFallback(slot("llmgateway", "minimax-m2.5")),
			Reasoning: ptr(slot("llmgateway", "glm-5")),
			Reviewing: slot("llmgateway", "deepseek-v3.2"),
			Judge:     slot("llmgateway", "gemini-2.5-flash"),
			Summarize: slot("llmgateway", "deepseek-v3.2"),
			Default:   slot("llmgateway", "deepseek-v3.2"),
			Fast:      ptr(slot("llmgateway", "gemini-2.5-flash-lite")),
		}
	case PresetOpenAI:
		r.RegisterProvider(OpenAIProvider(apiKey))
		r.Routing = ModelRouting{
			Coding:    slot("openai", "gpt-4o"),
			Reasoning: ptr(slot("openai", "o3-mini")),
			Reviewing: slot("openai", "gpt-4o"),
			Judge:     slot("openai", "gpt-4o-mini"),
			Summarize: slot("openai", "gpt-4o-mini"),
			Default:   slot("openai", "gpt-4o"),
			Fast:      ptr(slot("openai", "gpt-4o-mini")),
		}
	case PresetAnthropic:
		r.RegisterProvider(AnthropicProvider(apiKey))
		r.Routing = ModelRouting{
			Coding:    slot("anthropic", "claude-sonnet-4-20250514"),
			Reasoning: ptr(slot("anthropic", "claude-opus-4-20250514")),
			Reviewing: slot("anthropic", "claude-sonnet-4-20250514"),
			Judge:     slot("anthropic", "claude-haiku-4-5-20251001"),
			Summarize: slot("anthropic", "claude-haiku-4-5-20251001"),
			Default:   slot("anthropic", "claude-sonnet-4-20250514"),
			Fast:      ptr(slot("anthropic", "claude-haiku-4-5-20251001")),
		}
	}
	return r
}

// RegisterProvider adds (or replaces) a provider configuration.
func (r *ProviderRegistry) RegisterProvider(config ProviderConfig) {
	r.providers[config.ID] = config
}

// RemoveProvider drops a provider by ID.
func (r *ProviderRegistry) RemoveProvider(id string) {
	delete(r.providers, id)
}

// SetDefaultProvider points every routing slot at the given provider using its
// default model.
func (r *ProviderRegistry) SetDefaultProvider(providerID string) {
	model := ""
	if p, ok := r.providers[providerID]; ok {
		model = p.DefaultModel
	}
	r.Routing = uniformRouting(NewModelSlot(providerID, model))
}

// GetProvider looks up a provider by ID.
func (r *ProviderRegistry) GetProvider(id string) (ProviderConfig, bool) {
	p, ok := r.providers[id]
	return p, ok
}

// ListProviders returns every registered provider ID, sorted.
func (r *ProviderRegistry) ListProviders() []string {
	ids := make([]string, 0, len(r.providers))
	for id := range r.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// WithRouting installs a custom routing table and returns the registry.
func (r *ProviderRegistry) WithRouting(routing ModelRouting) *ProviderRegistry {
	r.Routing = routing
	return r
}

// resolveSlot resolves a slot to an LlmConfig, walking the fallback chain when
// the primary provider is not registered.
func (r *ProviderRegistry) resolveSlot(slot *ModelSlot) (LlmConfig, error) {
	if provider, ok := r.providers[slot.Provider]; ok {
		return LlmConfig{
			APIURL:      provider.APIURL,
			APIKey:      provider.APIKey,
			Model:       slot.Model,
			MaxTokens:   32768,
			Temperature: 0.0,
			APIFormat:   provider.APIFormat,
		}, nil
	}
	if slot.Fallback != nil {
		return r.resolveSlot(slot.Fallback)
	}
	return LlmConfig{}, fmt.Errorf("provider '%s' not registered and no fallback available", slot.Provider)
}

// LlmConfigFor resolves the route for an activity. It errors when the slot's
// provider — and every fallback — is unregistered, rather than silently
// substituting some other provider.
func (r *ProviderRegistry) LlmConfigFor(activity Activity) (LlmConfig, error) {
	return r.resolveSlot(r.Routing.SlotFor(activity))
}

// DefaultLlmConfig resolves the wire-compat `default` slot.
func (r *ProviderRegistry) DefaultLlmConfig() (LlmConfig, error) {
	return r.resolveSlot(&r.Routing.Default)
}

// ClientFor builds a gateway client for an activity's resolved route — the one
// line between "which model should this call use" and a client that speaks to it.
//
// The client is OpenAI-compatible; an Anthropic-format provider
// ([APIFormatAnthropic]) is rejected rather than silently spoken to in the wrong
// dialect.
func (r *ProviderRegistry) ClientFor(activity Activity) (*GatewayClient, LlmConfig, error) {
	config, err := r.LlmConfigFor(activity)
	if err != nil {
		return nil, LlmConfig{}, err
	}
	if config.APIFormat != APIFormatOpenAICompat {
		return nil, config, fmt.Errorf("activity %s routes to a %s provider, which the OpenAI-compatible gateway client cannot speak", activity, config.APIFormat)
	}
	return NewGatewayClient(config.APIURL, config.APIKey), config, nil
}

// LoadRegistryFromFile reads a registry from a JSON file (e.g. ~/.smooth/providers.json).
func LoadRegistryFromFile(path string) (*ProviderRegistry, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	registry, err := RegistryFromJSON(string(contents))
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return registry, nil
}

// RegistryFromJSON deserializes a registry from the same JSON shape
// [ProviderRegistry.SaveToFile] writes — used when a parent process passes the
// routing config to a child via an env var instead of a file.
func RegistryFromJSON(data string) (*ProviderRegistry, error) {
	var file registryFile
	if err := json.Unmarshal([]byte(data), &file); err != nil {
		return nil, fmt.Errorf("parsing provider registry JSON: %w", err)
	}
	registry := NewProviderRegistry().WithRouting(file.Routing)
	for _, provider := range file.Providers {
		registry.RegisterProvider(provider)
	}
	return registry, nil
}

// ToJSON serializes the registry to the shape [ProviderRegistry.SaveToFile] writes.
func (r *ProviderRegistry) ToJSON() (string, error) {
	data, err := json.Marshal(r.file())
	return string(data), err
}

// SaveToFile writes the registry as pretty-printed JSON, creating parent dirs.
func (r *ProviderRegistry) SaveToFile(path string) error {
	data, err := json.MarshalIndent(r.file(), "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func (r *ProviderRegistry) file() registryFile {
	providers := make([]ProviderConfig, 0, len(r.providers))
	for _, id := range r.ListProviders() {
		providers = append(providers, r.providers[id])
	}
	return registryFile{Providers: providers, Routing: r.Routing}
}

// RegistryFromEnv builds a minimal registry from SMOOTH_API_KEY (required),
// SMOOTH_PROVIDER (defaults to "openrouter") and SMOOTH_MODEL (optional).
// Returns false when SMOOTH_API_KEY is unset.
func RegistryFromEnv() (*ProviderRegistry, bool) {
	apiKey, ok := os.LookupEnv("SMOOTH_API_KEY")
	if !ok {
		return nil, false
	}
	providerID := os.Getenv("SMOOTH_PROVIDER")
	if providerID == "" {
		providerID = "openrouter"
	}

	var config ProviderConfig
	switch providerID {
	case "openai":
		config = OpenAIProvider(apiKey)
	case "anthropic":
		config = AnthropicProvider(apiKey)
	case "ollama":
		config = OllamaProvider()
		config.APIKey = apiKey
	case "google":
		config = GoogleProvider(apiKey)
	case "kimi":
		config = KimiProvider(apiKey)
	case "kimi-code":
		config = KimiCodeProvider(apiKey)
	case "llmgateway":
		config = LlmGatewayProvider(apiKey)
	default:
		config = OpenRouterProvider(apiKey)
	}

	model := os.Getenv("SMOOTH_MODEL")
	if model == "" {
		model = config.DefaultModel
	}

	registry := NewProviderRegistry()
	registry.RegisterProvider(config)
	registry.Routing = uniformRouting(NewModelSlot(providerID, model))
	return registry, true
}

// ---------------------------------------------------------------------------
// Per-model wire quirks
// ---------------------------------------------------------------------------

// ModelQuirks are per-model wire-format flags. Populate a field only when the
// quirk is worth the branch — every conditional is a place for drift.
//
// When routing through a LiteLLM-style gateway the concrete upstream model only
// reveals itself in response headers (`x-litellm-model-name`), by which point the
// request is already sent. So prefer always-safe request shapes over per-model
// conditionals, and keep this table for the cases where the strict form does not
// work everywhere.
type ModelQuirks struct {
	// AllowParallelTools, when non-nil and false, means the caller must force
	// `parallel_tool_calls` off even if the agent config requests it.
	AllowParallelTools *bool
	// StrictToolCallJSON asks the client to be extra careful about tool_call
	// echo shape — a few providers give obscure errors on borderline-malformed
	// echoes. Nothing reads this yet; it is the anchor for future tweaks.
	StrictToolCallJSON bool
}

type quirkEntry struct {
	needle string
	quirks ModelQuirks
}

func quirksTable() []quirkEntry {
	return []quirkEntry{
		{"qwen3-coder", ModelQuirks{StrictToolCallJSON: true}},
		{"qwen-coder", ModelQuirks{StrictToolCallJSON: true}},
	}
}

// QuirksForModel looks up quirks by concrete upstream name. Matching is
// case-insensitive and substring-based, so minor version drift
// (`qwen3-coder-plus-2025-04`) still hits the `qwen3-coder` entry. Returns the
// zero value when nothing matches, so defaults stay safe.
func QuirksForModel(upstream string) ModelQuirks {
	lc := strings.ToLower(upstream)
	for _, entry := range quirksTable() {
		if strings.Contains(lc, entry.needle) {
			return entry.quirks
		}
	}
	return ModelQuirks{}
}

// QuirkKeys lists the table's canonical keys, for diagnostics.
func QuirkKeys() []string {
	table := quirksTable()
	keys := make([]string, 0, len(table))
	for _, entry := range table {
		keys = append(keys, entry.needle)
	}
	return keys
}

// QuirksDebugSnapshot returns every quirk entry matching an upstream name.
// Usually one wins; the full set is kept so tests can assert coverage.
func QuirksDebugSnapshot(upstream string) map[string]ModelQuirks {
	out := map[string]ModelQuirks{}
	lc := strings.ToLower(upstream)
	for _, entry := range quirksTable() {
		if strings.Contains(lc, entry.needle) {
			out[entry.needle] = entry.quirks
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// LiteLLM alias resolution
// ---------------------------------------------------------------------------

// ResolvedModel is one routing entry returned by a gateway's `/model/info`.
type ResolvedModel struct {
	// Alias is the name callers use (e.g. `smooth-coding`).
	Alias string
	// Upstream is the concrete model (e.g. `moonshot/kimi-k2-thinking`), when
	// the gateway chose to surface it.
	Upstream string
	// ID is the stable id from `model_info.id`, when present — useful for
	// tracing a rename across the server-side model list.
	ID string
}

// BuildModelInfoURL derives the `/model/info` URL from a provider's
// OpenAI-compat api_url (e.g. https://llm.smoo.ai/v1). Stripping `/v1` is safe:
// `/model/info` lives at the gateway root in every LiteLLM deployment seen.
func BuildModelInfoURL(apiURL string) string {
	trimmed := strings.TrimRight(apiURL, "/")
	base := strings.TrimSuffix(trimmed, "/v1")
	return base + "/model/info"
}

type modelInfoDoc struct {
	Data []modelInfoEntry `json:"data"`
}

type modelInfoEntry struct {
	ModelName     string `json:"model_name"`
	LiteLlmParams struct {
		Model string `json:"model"`
	} `json:"litellm_params"`
	ModelInfo struct {
		ID string `json:"id"`
	} `json:"model_info"`
}

// ParseModelInfo parses a `/model/info` response body into an alias→entry map.
// Split out from the HTTP call so it is testable without a live gateway.
func ParseModelInfo(body string) (map[string]ResolvedModel, error) {
	var doc modelInfoDoc
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return nil, fmt.Errorf("parsing /model/info response: %w", err)
	}
	out := make(map[string]ResolvedModel, len(doc.Data))
	for _, entry := range doc.Data {
		out[entry.ModelName] = ResolvedModel{
			Alias:    entry.ModelName,
			Upstream: entry.LiteLlmParams.Model,
			ID:       entry.ModelInfo.ID,
		}
	}
	return out, nil
}

// SortedAliases returns a parsed map's aliases in sorted order, so diagnostics
// print the same order every run (Rust returns a BTreeMap; Go maps are unordered).
func SortedAliases(models map[string]ResolvedModel) []string {
	aliases := make([]string, 0, len(models))
	for alias := range models {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return aliases
}

// FetchModelInfo asks a LiteLLM gateway for its alias → upstream map.
//
// A 401 means the provider's API key is missing or rejected; either way the
// caller cannot see the mapping.
func FetchModelInfo(ctx context.Context, apiURL, apiKey string) (map[string]ResolvedModel, error) {
	url := BuildModelInfoURL(apiURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, string(raw))
	}
	return ParseModelInfo(string(raw))
}
