package core

// Ports the Rust reference engine's PromptCache tests (conversation.rs) and its
// cache_control wire tests (llm.rs).

import (
	"encoding/json"
	"strings"
	"testing"
)

// ── PromptCache (Rust: conversation.rs) ─────────────────────────────────────

func TestPromptCacheSplitsAtBoundary(t *testing.T) {
	c := NewPromptCache("static rules here" + PromptCacheBoundary + "dynamic context here")
	if c.StaticPortion() != "static rules here" {
		t.Errorf("static = %q", c.StaticPortion())
	}
	if c.DynamicPortion() != "dynamic context here" {
		t.Errorf("dynamic = %q", c.DynamicPortion())
	}
}

func TestPromptCacheNoMarkerTreatsAllAsDynamic(t *testing.T) {
	const prompt = "no marker in this prompt"
	c := NewPromptCache(prompt)
	if c.StaticPortion() != "" {
		t.Errorf("static should be empty, got %q", c.StaticPortion())
	}
	if c.DynamicPortion() != prompt {
		t.Errorf("dynamic = %q", c.DynamicPortion())
	}
}

func TestFullPromptCombinesStaticBoundaryDynamic(t *testing.T) {
	prompt := "You are an assistant." + PromptCacheBoundary + "Project: Smooth"
	if got := NewPromptCache(prompt).FullPrompt(); got != prompt {
		t.Errorf("FullPrompt() = %q, want %q", got, prompt)
	}
}

func TestUpdateDynamicOnlyChangesDynamicPortion(t *testing.T) {
	c := NewPromptCache("static" + PromptCacheBoundary + "old dynamic")
	original := c.StaticHash()

	c.UpdateDynamic("new dynamic")

	if c.DynamicPortion() != "new dynamic" {
		t.Errorf("dynamic = %q", c.DynamicPortion())
	}
	if c.StaticPortion() != "static" {
		t.Errorf("static = %q", c.StaticPortion())
	}
	if c.StaticHash() != original {
		t.Error("static hash must not change when only the dynamic half is swapped")
	}
}

func TestStaticHashIsDeterministic(t *testing.T) {
	prompt := "same static" + PromptCacheBoundary + "dynamic"
	if NewPromptCache(prompt).StaticHash() != NewPromptCache(prompt).StaticHash() {
		t.Error("same static text must hash the same")
	}
}

func TestStaticHashChangesWhenStaticChanges(t *testing.T) {
	a := NewPromptCache("static A" + PromptCacheBoundary + "dynamic")
	b := NewPromptCache("static B" + PromptCacheBoundary + "dynamic")
	if a.StaticHash() == b.StaticHash() {
		t.Error("different static text must hash differently")
	}
}

func TestCachedTokensReturnsStaticTokenEstimate(t *testing.T) {
	// "static text" is 11 chars => 11/4 + 1 = 3
	c := NewPromptCache("static text" + PromptCacheBoundary + "dynamic")
	if got := c.CachedTokens(); got != 11/4+1 {
		t.Errorf("CachedTokens() = %d, want %d", got, 11/4+1)
	}
	// No marker => empty static => 0 tokens.
	if got := NewPromptCache("all dynamic").CachedTokens(); got != 0 {
		t.Errorf("CachedTokens() with no static = %d, want 0", got)
	}
}

// ── cache_control gate (Rust: llm.rs) ───────────────────────────────────────

func TestCacheControlGateRecognizesClaudeRoutes(t *testing.T) {
	cases := []struct {
		model, url string
		want       bool
	}{
		// Claude model id + LiteLLM gateway url → cache it.
		{"claude-sonnet-4-20250514", "https://litellm.example.com/v1", true},
		// Smooth-coding alias + gateway url → cache it.
		{"smooth-coding-claude", "https://gateway.example.com/v1", true},
		// Direct Anthropic API + Claude id → cache it.
		{"claude-opus-4", "https://api.anthropic.com/v1", true},
		// GPT model on OpenAI → no cache control (would 400).
		{"gpt-4o", "https://api.openai.com/v1", false},
		// Gemini-compat → no cache control.
		{"gemini-1.5-pro", "https://generativelanguage.googleapis.com", false},
		// Claude id but bare OpenAI url (mis-configured) — still gated off.
		{"claude-3-sonnet", "https://api.openai.com/v1", false},
		// smooth-fast routes to Groq/Llama via the gateway — must NOT be cached.
		{"smooth-fast", "https://gateway.example.com/v1", false},
	}
	for _, c := range cases {
		if got := supportsAnthropicCacheControl(c.model, c.url); got != c.want {
			t.Errorf("supportsAnthropicCacheControl(%q, %q) = %v, want %v", c.model, c.url, got, c.want)
		}
	}
}

// ── cache_control wire shape (Rust: llm.rs) ─────────────────────────────────

func cacheTestRequest(model, url string) map[string]any {
	req := ChatRequest{
		Model: model,
		Messages: []ChatMessage{
			{Role: "system", Content: "You are smooth."},
			{Role: "user", Content: "Hi"},
		},
		Tools: []ToolSpec{
			{Name: "bash", Description: "Run a command", Parameters: map[string]any{}},
			{Name: "file_write", Description: "Write a file", Parameters: map[string]any{}},
		},
		MaxTokens: 100,
	}
	raw, err := json.Marshal(buildWireRequest(req, false, url))
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

func TestClaudeRequestBodyHasCacheControlOnSystemAndTools(t *testing.T) {
	body := cacheTestRequest("smooth-coding-claude", "https://gateway.example.com/v1")
	messages := body["messages"].([]any)

	// System message must be in block form with cache_control on its text block.
	sys := messages[0].(map[string]any)
	if sys["role"] != "system" {
		t.Fatalf("messages[0] role = %v", sys["role"])
	}
	sysContent, ok := sys["content"].([]any)
	if !ok {
		t.Fatalf("system content must be an array of blocks, got %#v", sys["content"])
	}
	sysBlock := sysContent[0].(map[string]any)
	if sysBlock["type"] != "text" || sysBlock["text"] != "You are smooth." {
		t.Fatalf("system block = %#v", sysBlock)
	}
	if cc := sysBlock["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Fatalf("system cache_control = %#v", cc)
	}

	// LAST tool must carry top-level cache_control; the first must not.
	tools := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools len = %d", len(tools))
	}
	if _, present := tools[0].(map[string]any)["cache_control"]; present {
		t.Error("first tool must not carry cache_control")
	}
	lastTool := tools[1].(map[string]any)
	if cc := lastTool["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Fatalf("last tool cache_control = %#v", cc)
	}

	// The user (last) message must also be marked so turn-by-turn caching extends.
	last := messages[1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("messages[1] role = %v", last["role"])
	}
	lastContent, ok := last["content"].([]any)
	if !ok {
		t.Fatalf("last message content must be in block form, got %#v", last["content"])
	}
	if cc := lastContent[0].(map[string]any)["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Fatalf("last message cache_control = %#v", cc)
	}
}

func TestGptRequestBodyHasNoCacheControl(t *testing.T) {
	req := ChatRequest{
		Model: "gpt-4o",
		Messages: []ChatMessage{
			{Role: "system", Content: "You are smooth."},
			{Role: "user", Content: "Hi"},
		},
		Tools:     []ToolSpec{{Name: "bash", Description: "Run a command", Parameters: map[string]any{}}},
		MaxTokens: 100,
	}
	raw, err := json.Marshal(buildWireRequest(req, false, "https://api.openai.com/v1"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if strings.Contains(body, "cache_control") {
		t.Errorf("GPT/OpenAI request body must NOT contain cache_control: %s", body)
	}
	// System content must serialize as a plain string for OpenAI compat.
	if !strings.Contains(body, `"content":"You are smooth."`) {
		t.Errorf("system content must be a plain string: %s", body)
	}
}

func TestNoAPIURLLeavesRequestUnmarked(t *testing.T) {
	// The gate can't fire without an api base, so an omitted URL must produce
	// exactly the pre-caching wire bytes even for a Claude model.
	req := ChatRequest{
		Model:     "claude-opus-4",
		Messages:  []ChatMessage{{Role: "system", Content: "You are smooth."}, {Role: "user", Content: "Hi"}},
		Tools:     []ToolSpec{{Name: "bash", Description: "Run a command", Parameters: map[string]any{}}},
		MaxTokens: 100,
	}
	withoutURL, err := json.Marshal(buildWireRequest(req, false))
	if err != nil {
		t.Fatal(err)
	}
	gatedOff, err := json.Marshal(buildWireRequest(req, false, "https://api.openai.com/v1"))
	if err != nil {
		t.Fatal(err)
	}
	if string(withoutURL) != string(gatedOff) {
		t.Errorf("omitting the api url must be byte-identical to a gated-off upstream:\n  %s\n  %s", withoutURL, gatedOff)
	}
	if strings.Contains(string(withoutURL), "cache_control") {
		t.Errorf("unmarked request must not contain cache_control: %s", withoutURL)
	}
}

func TestWrapWithCacheControlLeavesEmptyContentAlone(t *testing.T) {
	// A tool-call-only assistant message has no prose to cache; wrapping it would
	// emit an empty text block Anthropic has no use for.
	if got := wrapWithCacheControl(""); got != "" {
		t.Errorf("empty content must stay a plain empty string, got %#v", got)
	}
}

func TestWrapWithCacheControlRemarksLastBlockOnly(t *testing.T) {
	blocks := []wireTextBlock{
		{Type: "text", Text: "first", CacheControl: ephemeralCacheControl()},
		{Type: "text", Text: "second"},
	}
	out := wrapWithCacheControl(blocks).([]wireTextBlock)
	if out[0].CacheControl != nil {
		t.Error("re-marking must clear the stale marker on earlier blocks")
	}
	if out[1].CacheControl == nil || out[1].CacheControl.Type != "ephemeral" {
		t.Errorf("last block must carry the marker, got %#v", out[1].CacheControl)
	}
}
