package core

import (
	"context"
	"encoding/json"
	"fmt"
)

// Agent execution backend abstraction, plus the deterministic turn orchestration
// it dispatches to (ADR-030).
//
// An AgentExecutor is the seam that decides *where and how* an agent turn runs,
// while SmoothAgent remains the unit of orchestration (instructions, tools, loop
// policy, compaction, checkpointing). Today there is one implementation,
// InProcessExecutor, which drives the turn in the calling goroutine — identical
// behavior to calling SmoothAgent.Run / SmoothAgent.RunStream yourself. It
// carries no dependencies and needs no infrastructure: the zero-infra default.
//
// The point of the interface is to let an *optional* durable backend plug in
// without the engine or its consumers knowing. Such a backend models the turn as
// a durable workflow and runs the agent's side-effects (model calls, tool
// invocations) as activities, giving crash-safe resume, durable
// human-in-the-loop, and durable timers. The in-process executor runs those same
// side-effects inline; the interface boundary is what lets the two be swapped at
// the edge.
//
// Keeping this a thin dispatch seam (rather than reaching into the loop) is
// deliberate: the in-process path stays a verbatim delegation to the existing,
// battle-tested Run / RunStream surface, so introducing the abstraction changes
// no behavior. The finer orchestration-vs-activity split a durable backend needs
// lives below, in AgentActivities + DriveTurn.
//
// TODO(ADR-030): a Temporal-backed AgentExecutor / AgentActivities pair belongs
// in a SEPARATE package behind an opt-in build tag (mirroring the Rust
// `smooth-operator-temporal` crate + its `temporal` cargo feature), so the
// default build of this package stays zero-infra and dependency-free.

// AgentExecutor is a backend that executes one SmoothAgent turn.
//
// Implementations decide *how* the turn is driven (inline, or on a durable
// workflow engine); the agent passed in owns *what* the turn does. Both methods
// take the agent as an argument so an executor never owns it — it borrows it for
// the duration of the turn, exactly like calling Run directly.
type AgentExecutor interface {
	// Execute runs a single turn for message, with history as the prior
	// conversation. Behaviorally equivalent to SmoothAgent.Run for the in-process
	// backend. It propagates any fatal error from the underlying turn (model call
	// or — for durable backends — the workflow engine).
	Execute(ctx context.Context, agent *SmoothAgent, message string, history []ChatMessage) (AgentRunResponse, error)

	// ExecuteStreaming runs a single turn, delivering StreamEvents (text deltas,
	// tool call/result, the terminal StreamDone) on the returned Stream.
	// Behaviorally equivalent to SmoothAgent.RunStream for the in-process backend,
	// including its error contract: a mid-turn failure closes the event channel
	// without a StreamDone and is reported by Stream.Err.
	ExecuteStreaming(ctx context.Context, agent *SmoothAgent, message string, thread *SmoothAgentThread) (*Stream, error)
}

// InProcessExecutor is the default, zero-infra executor: it drives the turn in
// the calling goroutine by delegating straight to SmoothAgent.Run /
// SmoothAgent.RunStream.
//
// This is a verbatim pass-through — introducing it changes no behavior. It is
// the executor used unless a consumer explicitly opts into a durable backend.
type InProcessExecutor struct{}

// NewInProcessExecutor constructs the in-process executor. (It is a zero-size
// struct; the constructor exists for symmetry with stateful executors.)
func NewInProcessExecutor() *InProcessExecutor { return &InProcessExecutor{} }

func (*InProcessExecutor) Execute(ctx context.Context, agent *SmoothAgent, message string, history []ChatMessage) (AgentRunResponse, error) {
	return agent.Run(ctx, message, history)
}

func (*InProcessExecutor) ExecuteStreaming(ctx context.Context, agent *SmoothAgent, message string, thread *SmoothAgentThread) (*Stream, error) {
	return agent.RunStream(ctx, message, thread)
}

// AgentActivities is the side-effecting boundary of an agent turn: the model
// call and each tool invocation. These are exactly the steps a durable backend
// runs as Temporal activities (retried, memoized in workflow history); the
// in-process backend runs them inline.
//
// Inputs and outputs are owned, serializable values (not views into agent state)
// so an implementation may marshal them across an activity boundary unchanged.
type AgentActivities interface {
	// ModelCall invokes the model with the given context window and the tool
	// specs currently visible to it. It propagates a fatal model-call error
	// (network, upstream rejection); a durable backend turns transient failures
	// into activity retries before they surface here.
	ModelCall(ctx context.Context, messages []ChatMessage, tools []ToolSpec) (ChatResponse, error)

	// ToolInvoke executes a single tool call and returns its result. Tool
	// *business* failures — unknown tool, bad arguments, a tool that returned an
	// error — are reported on ToolResult.IsError, NOT as a returned error, exactly
	// as the agent's own dispatch does; only a fatal dispatch error is an error.
	// That distinction matters for a durable backend: an error is retried as a
	// failed activity, while an IsError result is a completed activity whose
	// content goes back to the model.
	ToolInvoke(ctx context.Context, call ToolCall) (ToolResult, error)
}

// TurnPolicy is the loop policy for DriveTurn: the bound that keeps the
// orchestration terminating. Mirrors AgentOptions.MaxIterations.
type TurnPolicy struct {
	// MaxIterations is the maximum number of model-call iterations before the
	// turn stops unconditionally.
	MaxIterations int
}

// DefaultTurnPolicy returns the policy the agent's own loop uses when
// AgentOptions.MaxIterations is unset. NOTE this is 20 here, not the Rust
// engine's 50 — each core keeps its own loop bound, and DriveTurn tracks the Go
// agent's so the two Go paths stay identical.
func DefaultTurnPolicy() TurnPolicy { return TurnPolicy{MaxIterations: defaultMaxIterations} }

// DriveTurn drives one agent turn deterministically over the supplied activity
// surface, returning the conversation with this turn's assistant and tool
// messages appended.
//
// messages must already be seeded with the system prompt, any prior turns, and
// the current user message — exactly the state SmoothAgent.run holds when it
// enters its loop. The input slice is never mutated in place.
//
// This function performs NO I/O, wall-clock reads, or randomness of its own —
// all of that is delegated to activities — so it is replay-safe: a durable
// workflow can run this very function, and the in-process path runs it too. One
// loop, two backends, which is what keeps the durable path from diverging from
// the inline one.
//
// It reproduces the core decision flow of SmoothAgent.run (snapshot context →
// model call → append assistant message → stop-or-run-tools → append tool
// results → loop to MaxIterations). It deliberately omits the inline-runtime
// concerns a durable backend models differently: event emission (a UI concern —
// durable backends surface progress from workflow history), checkpointing
// (Temporal's event history *is* the checkpoint), and the compaction / budget /
// parallel-dispatch / deferred-tool layers, which fold onto this loop without
// changing its shape.
func DriveTurn(ctx context.Context, activities AgentActivities, messages []ChatMessage, tools []ToolSpec, policy TurnPolicy) ([]ChatMessage, error) {
	convo := append([]ChatMessage(nil), messages...)

	for iteration := 1; iteration <= policy.MaxIterations; iteration++ {
		// Observe: snapshot the context window as owned messages so the call can
		// cross an activity boundary.
		window := append([]ChatMessage(nil), convo...)

		// Think.
		resp, err := activities.ModelCall(ctx, window, tools)
		if err != nil {
			return convo, fmt.Errorf("model call: %w", err)
		}

		// Append the assistant turn (carrying any tool calls), mirroring the Rust
		// engine's push condition. Go's ChatResponse has no reasoning-content
		// field, so the third arm of that condition has nothing to test here.
		if resp.Content != "" || len(resp.ToolCalls) > 0 {
			convo = append(convo, ChatMessage{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})
		}

		// No tool calls ⇒ the agent is done.
		if len(resp.ToolCalls) == 0 {
			return convo, nil
		}

		// Act: run each tool in order and append its result, paired to the call by
		// id so the model can match results to calls. (Go's ChatMessage carries the
		// call id only — the agent's own loop pairs the same way.)
		for _, call := range resp.ToolCalls {
			result, err := activities.ToolInvoke(ctx, call)
			if err != nil {
				return convo, fmt.Errorf("tool invoke %q: %w", call.Name, err)
			}
			convo = append(convo, ChatMessage{Role: "tool", ToolCallID: call.ID, Content: result.Content})
		}
	}

	// Iteration budget exhausted mid-tool-chain — return what we have, matching
	// the agent loop's post-loop behavior (exhaustion is not an error).
	return convo, nil
}

// InProcessActivities is the default, zero-infra AgentActivities: it runs the
// model call through an LlmProvider and tool calls through the agent's tool set,
// inline in the calling goroutine.
type InProcessActivities struct {
	llm   LlmProvider
	model string
	tools map[string]Tool
}

// NewInProcessActivities builds an in-process activity surface from a model
// provider, the model to call, and the tools available to it. An empty model
// falls back to the package default, matching the agent.
func NewInProcessActivities(llm LlmProvider, model string, tools []Tool) *InProcessActivities {
	if model == "" {
		model = defaultModel
	}
	byName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		byName[t.Name()] = t
	}
	return &InProcessActivities{llm: llm, model: model, tools: byName}
}

func (a *InProcessActivities) ModelCall(ctx context.Context, messages []ChatMessage, tools []ToolSpec) (ChatResponse, error) {
	return a.llm.Chat(ctx, ChatRequest{Model: a.model, Messages: messages, Tools: tools, MaxTokens: defaultMaxTokens})
}

// ToolInvoke resolves and runs one tool. Every failure mode is a completed
// invocation reported on ToolResult.IsError — the model is told what went wrong
// and can adapt — so this never returns a non-nil error, mirroring the agent's
// dispatchToolResult.
func (a *InProcessActivities) ToolInvoke(ctx context.Context, call ToolCall) (ToolResult, error) {
	errResult := func(msg string) (ToolResult, error) {
		return ToolResult{ToolCallID: call.ID, Content: msg, IsError: true}, nil
	}
	tool, ok := a.tools[call.Name]
	if !ok {
		return errResult(fmt.Sprintf("error: unknown tool '%s'", call.Name))
	}
	args := map[string]any{}
	if call.Arguments != "" {
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return errResult(fmt.Sprintf("error: tool '%s' received invalid JSON arguments", call.Name))
		}
	}
	out, err := tool.Execute(ctx, args)
	if err != nil {
		return errResult(fmt.Sprintf("error: tool '%s' failed: %v", call.Name, err))
	}
	return ToolResult{ToolCallID: call.ID, Content: out}, nil
}
