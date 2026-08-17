// Package temporal is the optional Temporal-backed durable execution backend for
// the smooth-operator Go agent engine (ADR-030).
//
// An agent turn runs as the [AgentTurnWorkflow], whose side-effects — the model
// call and each tool invocation — are Temporal activities ([AgentTurnActivities]).
// The workflow drives the engine's deterministic [core.DriveTurn] orchestration
// UNCHANGED via a workflow-side adapter that schedules those activities, so the
// durable path and the in-process path are the same loop — exactly like the Rust
// `smooth-operator-temporal` crate.
//
// This lives in its own Go module so the Temporal SDK is opt-in: the engine's
// default build depends on none of it (the mirror of the Rust crate's `temporal`
// cargo feature).
package temporal

import core "github.com/SmooAI/smooth-operator-core/go/core"

// ModelCallInput is the input to the ModelCall activity: the context window plus
// the tools visible to the model. It is the serialized boundary the durable path
// marshals across — mirrors the Rust `dto::ModelCallInput`.
type ModelCallInput struct {
	// Messages is the conversation context window, as owned messages.
	Messages []core.ChatMessage `json:"messages"`
	// Tools are the tool specs the model may call.
	Tools []core.ToolSpec `json:"tools"`
}

// ToolInvokeInput is the input to the ToolInvoke activity: the single tool call to
// dispatch. Mirrors the Rust `dto::ToolInvokeInput`.
type ToolInvokeInput struct {
	// Call is the tool call (id + name + raw-JSON arguments) to execute.
	Call core.ToolCall `json:"call"`
}

// ponytail: The Rust reference also carries a `ModelCallOutput` projection because
// its `LlmResponse` is deliberately NOT serde-serializable (it holds transient
// gateway state). Go's core.ChatResponse has no such transient field — every field
// is exported and JSON-serializable — so the model-call activity returns
// core.ChatResponse DIRECTLY as its output DTO, and no hand-written projection is
// needed. The serde boundary that crosses the activity is exactly ModelCallInput,
// ToolInvokeInput, and core.ChatResponse; dto_test.go round-trips all three without
// a Temporal server.
