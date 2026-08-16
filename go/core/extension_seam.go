package core

import (
	"context"
	"encoding/json"
)

// ExtensionHooks is the seam through which a SEP extension host participates in
// the agent loop — the Go sibling of Rust's `Agent::with_extension_host`.
//
// It is an INTERFACE rather than a concrete `*extension.ExtensionHost` because
// the dependency runs the other way: `go/core/extension` imports `go/core` (an
// `ExtensionTool` is a `core.Tool`), so `core` importing `extension` back would
// be a cycle. Every signature here is expressible in `core` + stdlib alone, and
// `extension.NewAgentBridge(host)` supplies the implementation.
//
// Nil (the default) means the agent loop behaves exactly as it did before
// extensions existed — no dispatch, no hook calls, no allocation.
type ExtensionHooks interface {
	// ExtensionTools are the host's eager tools, already namespaced
	// `<extension>.<tool>`. They are merged into the agent's tool set as
	// ORDINARY tools, so they are visible to the model, dispatched, and gated by
	// exactly the same permission machinery as native tools — no special casing.
	ExtensionTools() []Tool

	// ExtensionDeferredTools are the host's deferred tools: hidden from the model
	// until `tool_search` promotes them.
	ExtensionDeferredTools() []Tool

	// RunToolCallHook folds the `tool_call` hook chain over one pending call
	// BEFORE it executes. Returns whether the call is vetoed (with the reason),
	// and the possibly-rewritten arguments to run with.
	//
	// Rewrites are already scoped by the host's cross-tool guard: an extension
	// may only rewrite the arguments of a tool it owns, and may never redirect
	// the call to a different tool.
	RunToolCallHook(ctx context.Context, tool string, arguments string) (blocked bool, reason string, patched string)

	// DispatchEvent fans a turn event out to subscribed extensions. Fire and
	// forget: observe events are lossy by contract and never block the turn.
	DispatchEvent(event string, payload json.RawMessage)
}

// SEP event names the agent loop emits. Mirrors the Rust host's
// `extension::events` constants.
const (
	sepTurnStart  = "turn_start"
	sepTurnEnd    = "turn_end"
	sepMessageEnd = "message_end"
)

// sepDispatch is the nil-safe event fan-out used throughout the loop.
func (a *SmoothAgent) sepDispatch(event string, payload any) {
	if a.options.Extensions == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return // a payload that won't marshal is not worth failing a turn over
	}
	a.options.Extensions.DispatchEvent(event, raw)
}

// sepTurnComplete emits the pair of end-of-turn events, in the order Rust emits
// them: `message_end` carrying the final assistant text, then `turn_end`.
func (a *SmoothAgent) sepTurnComplete(iterations int, content string) {
	if a.options.Extensions == nil {
		return
	}
	a.sepDispatch(sepMessageEnd, map[string]any{"iteration": iterations, "content": content})
	a.sepDispatch(sepTurnEnd, map[string]any{"agent_id": a.options.Model, "iterations": iterations})
}

// sepBlockedResult is what the model sees in place of a vetoed call's result.
// Phrased so the model learns the call was refused and why, rather than seeing
// an empty result and retrying blindly.
func sepBlockedResult(reason string) string {
	if reason == "" {
		return "error: blocked by extension"
	}
	return "error: blocked by extension: " + reason
}

// sepToolCallPlan folds the `tool_call` hook over every pending call before any
// of them execute — the Go sibling of Rust's `sep_tool_call_plan`.
//
// Returns the calls to actually run (arguments possibly rewritten) and, for any
// vetoed call, its id → reason. With no host configured it returns the input
// untouched and a nil map, so the no-extension path allocates nothing extra.
func (a *SmoothAgent) sepToolCallPlan(ctx context.Context, calls []ToolCall) ([]ToolCall, map[string]string) {
	if a.options.Extensions == nil || len(calls) == 0 {
		return calls, nil
	}
	out := make([]ToolCall, len(calls))
	var blocks map[string]string
	for i, tc := range calls {
		out[i] = tc
		blocked, reason, patched := a.options.Extensions.RunToolCallHook(ctx, tc.Name, tc.Arguments)
		if blocked {
			if blocks == nil {
				blocks = make(map[string]string, 1)
			}
			blocks[tc.ID] = reason
			continue
		}
		if patched != "" {
			out[i].Arguments = patched
		}
	}
	return out, blocks
}
