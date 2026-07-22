package core

import "context"

// ToolResult is the outcome of a single tool call. It is handed to
// ToolHook.PostCall as a mutable pointer so a hook can rewrite Content before it
// reaches the model — the redaction seam (e.g. scrubbing a leaked secret from a
// tool's output). Mirrors the Rust engine's ToolResult.
type ToolResult struct {
	// ToolCallID is the id of the ToolCall this result answers.
	ToolCallID string
	// Content is the text the model sees. A PostCall hook may mutate it.
	Content string
	// IsError is true when the tool failed (or a PreCall hook blocked it).
	IsError bool
}

// ToolHook observes and can gate/redact tool calls, mirroring the Rust engine's
// ToolHook lifecycle (pre_call / post_call). Hooks are registered via
// AgentOptions.Hooks and run in registration order around every dispatched tool
// call. A hook must be safe for concurrent use: with ParallelToolCalls the
// engine may run PreCall/PostCall from multiple goroutines at once.
type ToolHook interface {
	// PreCall runs before a tool executes. Returning a non-nil error blocks the
	// call: the tool never runs and the model is told it was blocked.
	PreCall(ctx context.Context, call ToolCall) error
	// PostCall runs after a tool executes, with a mutable *ToolResult it may
	// rewrite (the redaction seam). A returned error is ignored — the
	// (possibly redacted) result still reaches the model — matching the Rust
	// engine, where a post-hook failure is logged, not surfaced.
	PostCall(ctx context.Context, call ToolCall, result *ToolResult) error
}
