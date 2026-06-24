package core

import (
	"context"
	"encoding/json"
	"strings"
)

// Phase-3 sibling of the Rust reference tool_search.rs. Mirrors the behaviour,
// not the type shapes (this core has no ToolRegistry — tools are a plain slice
// on AgentOptions).
//
// As a tool set grows past ~20-30 entries, every model turn pays tokens to read
// schemas it isn't going to use, diluting the model's attention budget. So a
// caller can register some tools as deferred (AgentOptions.DeferredTools): their
// schemas are hidden from the model. Instead the agent advertises a single
// built-in tool_search(query) meta-tool. When the model calls it, this
// fuzzy-matches the query against the deferred tools' names + descriptions,
// promotes the matches into the visible set (so the model can call them on
// subsequent turns), and returns each match's name + description as JSON.
//
// A deferred tool that has not been promoted is NOT dispatchable — calling it
// surfaces as an unknown tool until tool_search adds it to the promoted set.

// toolSearchName is the built-in meta-tool's name. Reserved when deferred tools
// are in play.
const toolSearchName = "tool_search"

// maxToolSearchMatches caps how many deferred tools a single tool_search call may
// promote, so a generic query like "tool" doesn't promote the whole deferred set.
const maxToolSearchMatches = 8

const toolSearchDescription = "Search for additional tools by keyword. Returns matching tool schemas as JSON; " +
	"matched tools become available on subsequent turns. Use when you think a tool " +
	"exists for a specific task but isn't in your current tool list — e.g. " +
	`tool_search(query="git") or tool_search(query="http request").`

// ToolSearch drives deferred-tool promotion for one agent run. It implements Tool
// so the agent can advertise + dispatch it like any other tool. It holds the
// deferred tools (by name) and the mutable set of promoted names; the agent
// consults PromotedTools each iteration to decide which deferred schemas are now
// visible/dispatchable.
type ToolSearch struct {
	deferredByName map[string]Tool
	order          []string // registration order, for stable match truncation
	promoted       map[string]bool
}

// NewToolSearch builds a ToolSearch over the given deferred tools.
func NewToolSearch(deferred []Tool) *ToolSearch {
	byName := make(map[string]Tool, len(deferred))
	order := make([]string, 0, len(deferred))
	for _, t := range deferred {
		if _, ok := byName[t.Name()]; !ok {
			order = append(order, t.Name())
		}
		byName[t.Name()] = t
	}
	return &ToolSearch{deferredByName: byName, order: order, promoted: map[string]bool{}}
}

func (s *ToolSearch) Name() string        { return toolSearchName }
func (s *ToolSearch) Description() string { return toolSearchDescription }
func (s *ToolSearch) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Keyword to match against deferred tool names and descriptions. Case-insensitive substring match.",
			},
		},
		"required": []string{"query"},
	}
}

// HasDeferred reports whether any tool was registered deferred (the meta-tool is
// advertised only then).
func (s *ToolSearch) HasDeferred() bool { return len(s.deferredByName) > 0 }

// IsPromoted reports whether a deferred tool has been promoted and is now
// dispatchable.
func (s *ToolSearch) IsPromoted(name string) bool { return s.promoted[name] }

// PromotedTools returns the deferred tools that have been promoted — their schemas
// join the visible set. Returned in registration order for determinism.
func (s *ToolSearch) PromotedTools() []Tool {
	out := make([]Tool, 0, len(s.promoted))
	for _, name := range s.order {
		if s.promoted[name] {
			out = append(out, s.deferredByName[name])
		}
	}
	return out
}

// ToolByName resolves a promoted deferred tool for dispatch. Unpromoted deferred
// tools are invisible (returns nil, false).
func (s *ToolSearch) ToolByName(name string) (Tool, bool) {
	if s.promoted[name] {
		t, ok := s.deferredByName[name]
		return t, ok
	}
	return nil, false
}

// Promote marks a deferred tool promoted. Returns false if no such deferred tool.
func (s *ToolSearch) Promote(name string) bool {
	if _, ok := s.deferredByName[name]; !ok {
		return false
	}
	s.promoted[name] = true
	return true
}

// Execute fuzzy-matches the query, promotes matches, and returns their schemas as
// JSON.
func (s *ToolSearch) Execute(_ context.Context, args map[string]any) (string, error) {
	query, ok := args["query"].(string)
	if !ok {
		return marshalToolSearchResult(0, nil, "missing required `query` parameter"), nil
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return marshalToolSearchResult(0, nil, `empty query — pass a keyword like "git" or "network"`), nil
	}

	matched := make([]Tool, 0, maxToolSearchMatches)
	for _, name := range s.order {
		t := s.deferredByName[name]
		if strings.Contains(strings.ToLower(t.Name()), needle) || strings.Contains(strings.ToLower(t.Description()), needle) {
			matched = append(matched, t)
			if len(matched) >= maxToolSearchMatches {
				break
			}
		}
	}

	for _, t := range matched {
		s.promoted[t.Name()] = true
	}

	return marshalToolSearchResult(len(matched), matched, ""), nil
}

// marshalToolSearchResult renders the tool_search JSON payload. The note field is
// omitted when empty.
func marshalToolSearchResult(matched int, tools []Tool, note string) string {
	specs := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		specs = append(specs, map[string]any{
			"name":        t.Name(),
			"description": t.Description(),
			"parameters":  t.Parameters(),
		})
	}
	payload := map[string]any{"matched": matched, "tools": specs}
	if note != "" {
		payload["note"] = note
	}
	b, _ := json.Marshal(payload)
	return string(b)
}
