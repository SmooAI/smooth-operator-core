package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func funcTool(name, description string) Tool {
	return FuncTool{
		ToolName: name,
		Desc:     description,
		Params:   map[string]any{"type": "object"},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			return "ran " + name, nil
		},
	}
}

type toolSearchResult struct {
	Matched int `json:"matched"`
	Tools   []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

func parseToolSearch(t *testing.T, raw string) toolSearchResult {
	t.Helper()
	var r toolSearchResult
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("bad tool_search payload %q: %v", raw, err)
	}
	return r
}

func specNames(req ChatRequest) []string {
	names := make([]string, len(req.Tools))
	for i, s := range req.Tools {
		names[i] = s.Name
	}
	return names
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// ── unit tests on ToolSearch directly ────────────────────────────────────────
func TestToolSearchMatchesByNameAndPromotes(t *testing.T) {
	s := NewToolSearch([]Tool{
		funcTool("git_status", "Show git working tree status"),
		funcTool("git_diff", "Show git diff between commits"),
		funcTool("http_get", "Fetch a URL via HTTP GET"),
	})
	out, err := s.Execute(context.Background(), map[string]any{"query": "git"})
	if err != nil {
		t.Fatal(err)
	}
	r := parseToolSearch(t, out)
	if r.Matched != 2 {
		t.Fatalf("matched = %d, want 2", r.Matched)
	}
	if !s.IsPromoted("git_status") || !s.IsPromoted("git_diff") {
		t.Fatalf("git tools should be promoted")
	}
	if s.IsPromoted("http_get") {
		t.Fatalf("http_get should remain deferred")
	}
}

func TestToolSearchMatchesByDescriptionCaseInsensitive(t *testing.T) {
	s := NewToolSearch([]Tool{funcTool("http_get", "Fetch a URL via HTTP GET")})
	out, _ := s.Execute(context.Background(), map[string]any{"query": "url"})
	r := parseToolSearch(t, out)
	if r.Matched != 1 || !s.IsPromoted("http_get") {
		t.Fatalf("expected http_get matched+promoted, got matched=%d promoted=%v", r.Matched, s.IsPromoted("http_get"))
	}
}

func TestToolSearchNoMatchPromotesNothing(t *testing.T) {
	s := NewToolSearch([]Tool{funcTool("git_status", "Show git status")})
	out, _ := s.Execute(context.Background(), map[string]any{"query": "xyzzy"})
	r := parseToolSearch(t, out)
	if r.Matched != 0 || len(r.Tools) != 0 {
		t.Fatalf("expected no matches, got %+v", r)
	}
	if s.IsPromoted("git_status") {
		t.Fatalf("nothing should be promoted")
	}
}

func TestToolSearchEmptyQueryIsNoop(t *testing.T) {
	s := NewToolSearch([]Tool{funcTool("git_status", "Show git status")})
	out, _ := s.Execute(context.Background(), map[string]any{"query": "   "})
	r := parseToolSearch(t, out)
	if r.Matched != 0 || s.IsPromoted("git_status") {
		t.Fatalf("empty query should be a no-op, got %+v promoted=%v", r, s.IsPromoted("git_status"))
	}
}

// ── end-to-end through the agent loop ────────────────────────────────────────
func TestDeferredSchemaHiddenUntilPromotedThenDispatchable(t *testing.T) {
	client := &fakeClient{scripted: []ChatResponse{
		// Turn 1: model searches for git tools.
		{ToolCalls: []ToolCall{{ID: "c1", Name: "tool_search", Arguments: `{"query":"git"}`}}},
		// Turn 2: model calls the now-promoted git_status tool.
		{ToolCalls: []ToolCall{{ID: "c2", Name: "git_status", Arguments: `{}`}}},
		// Turn 3: model wraps up.
		{Content: "done"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{
		Tools:         []Tool{funcTool("echo", "Echo back")},
		DeferredTools: []Tool{funcTool("git_status", "Show git working tree status"), funcTool("http_get", "Fetch a URL via HTTP GET")},
	})
	res, err := agent.Run(context.Background(), "inspect the repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" || res.ToolCalls != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}

	// Turn 1: eager tool + tool_search advertised; deferred tools hidden.
	first := specNames(client.calls[0])
	if !contains(first, "echo") || !contains(first, "tool_search") {
		t.Fatalf("turn 1 should advertise echo + tool_search, got %v", first)
	}
	if contains(first, "git_status") || contains(first, "http_get") {
		t.Fatalf("deferred tools must stay hidden on turn 1, got %v", first)
	}
	// Turn 2: git_status promoted into view; http_get still hidden.
	second := specNames(client.calls[1])
	if !contains(second, "git_status") {
		t.Fatalf("git_status should be promoted into view on turn 2, got %v", second)
	}
	if contains(second, "http_get") {
		t.Fatalf("http_get should still be hidden, got %v", second)
	}
	// The promoted tool actually dispatched, fed back as a tool message on the
	// turn-3 request (the result of the turn-2 git_status call).
	found := false
	for _, m := range client.calls[2].Messages {
		if m.Role == "tool" && m.Content == "ran git_status" {
			found = true
		}
	}
	if !found {
		t.Fatalf("promoted git_status did not dispatch; messages=%+v", client.calls[2].Messages)
	}
}

func TestUnpromotedDeferredToolIsNotDispatchable(t *testing.T) {
	client := &fakeClient{scripted: []ChatResponse{
		// Model jumps straight to a deferred tool it was never shown — should fail.
		{ToolCalls: []ToolCall{{ID: "c1", Name: "git_status", Arguments: `{}`}}},
		{Content: "ok"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{DeferredTools: []Tool{funcTool("git_status", "Show git working tree status")}})
	if _, err := agent.Run(context.Background(), "try it", nil); err != nil {
		t.Fatal(err)
	}
	var toolMsg *ChatMessage
	for i := range client.calls[1].Messages {
		if client.calls[1].Messages[i].Role == "tool" {
			toolMsg = &client.calls[1].Messages[i]
			break
		}
	}
	if toolMsg == nil || !strings.Contains(toolMsg.Content, "unknown tool 'git_status'") {
		t.Fatalf("unpromoted deferred tool should resolve to unknown-tool error, got %+v", toolMsg)
	}
}

func TestNoDeferredToolsMeansNoMetaTool(t *testing.T) {
	client := &fakeClient{scripted: []ChatResponse{{Content: "hi"}}}
	agent := NewSmoothAgent(client, AgentOptions{Tools: []Tool{funcTool("echo", "echo")}})
	if _, err := agent.Run(context.Background(), "hello", nil); err != nil {
		t.Fatal(err)
	}
	if contains(specNames(client.calls[0]), "tool_search") {
		t.Fatalf("tool_search must not be advertised without deferred tools, got %v", specNames(client.calls[0]))
	}
}
