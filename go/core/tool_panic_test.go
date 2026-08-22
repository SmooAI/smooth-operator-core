package core

import (
	"context"
	"strings"
	"testing"
)

// panicTool is a host tool that panics instead of returning — the shape of a
// buggy tool (nil map write, index out of range) rather than one that reports an
// error. Without recover() at the Execute call site this unwinds the goroutine,
// and Go escalates an unrecovered panic to a process-wide crash: every one of
// these tests kills the whole `go test` binary if the guard is removed.
func panicTool(name string) FuncTool {
	return FuncTool{
		ToolName: name,
		Desc:     "Panics",
		Params:   map[string]any{"type": "object", "properties": map[string]any{}},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			var m map[string]string
			m["boom"] = "kaboom" // assignment to entry in nil map
			return "unreachable", nil
		},
	}
}

// A panicking tool must fail THAT tool call and nothing else: the turn keeps
// running, the model gets an IsError result it can react to, and the process
// lives. Matches TypeScript/Python core, whose catch around execute() already
// converts a throwing tool into an error result.
func TestPanickingToolFailsOnlyItsCall(t *testing.T) {
	mock := NewMockLlmProvider()
	mock.PushResponse(ToolCallResponse("call-1", "boom", `{}`)).
		PushResponse(TextResponse("recovered and replied"))
	agent := NewSmoothAgent(mock, AgentOptions{Tools: []Tool{panicTool("boom")}})

	resp, err := agent.Run(context.Background(), "call boom", nil)
	if err != nil {
		t.Fatalf("a panicking tool must not fail the turn: %v", err)
	}
	if resp.Text != "recovered and replied" {
		t.Fatalf("turn should have continued past the panic, got %q", resp.Text)
	}
	// The model must be able to SEE the failure, not just be handed an empty result.
	res := agent.dispatchToolResult(context.Background(), ToolCall{ID: "x", Name: "boom", Arguments: `{}`}, nil)
	if !res.IsError {
		t.Fatalf("panic must surface as an error result, got %+v", res)
	}
	if !strings.Contains(res.Content, "panicked") || !strings.Contains(res.Content, "boom") {
		t.Fatalf("result must name the tool and the panic, got %q", res.Content)
	}
}

// ParallelToolCalls dispatches each tool on its own goroutine, where a
// caller-side recover cannot reach the panic — recover() only catches panics on
// the goroutine that deferred it. This is why the guard belongs at the Execute
// call site and not in Run/RunStream.
func TestPanickingToolInParallelDispatch(t *testing.T) {
	ok := FuncTool{
		ToolName: "ok",
		Desc:     "Works",
		Params:   map[string]any{"type": "object", "properties": map[string]any{}},
		Fn:       func(_ context.Context, _ map[string]any) (string, error) { return "fine", nil },
	}
	mock := NewMockLlmProvider()
	mock.PushResponse(ChatResponse{ToolCalls: []ToolCall{
		{ID: "c1", Name: "boom", Arguments: `{}`},
		{ID: "c2", Name: "ok", Arguments: `{}`},
	}}).PushResponse(TextResponse("done"))
	agent := NewSmoothAgent(mock, AgentOptions{
		Tools:             []Tool{panicTool("boom"), ok},
		ParallelToolCalls: true,
	})

	resp, err := agent.Run(context.Background(), "call both", nil)
	if err != nil {
		t.Fatalf("parallel dispatch must survive a panicking sibling: %v", err)
	}
	if resp.Text != "done" {
		t.Fatalf("turn should have completed, got %q", resp.Text)
	}
}

// Same guarantee on the streaming path, which additionally runs the whole turn on
// a goroutine RunStream owns.
func TestPanickingToolDuringRunStream(t *testing.T) {
	mock := NewMockLlmProvider()
	mock.PushResponse(ToolCallResponse("call-1", "boom", `{}`)).
		PushResponse(TextResponse("streamed on"))
	agent := NewSmoothAgent(mock, AgentOptions{Tools: []Tool{panicTool("boom")}})

	stream, err := agent.RunStream(context.Background(), "call boom", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := drain(stream)
	if err != nil {
		t.Fatalf("a panicking tool must not abort the stream: %v", err)
	}
	var toolResult string
	sawDone := false
	for _, e := range events {
		switch e.Kind {
		case StreamToolResult:
			toolResult = e.Result
		case StreamDone:
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("stream must still reach StreamDone")
	}
	if !strings.Contains(toolResult, "panicked") {
		t.Fatalf("streamed tool result must carry the panic, got %q", toolResult)
	}
}

// panickyHook panics OUTSIDE the tool-execute guard, on the goroutine RunStream
// owns. That is the backstop case: the turn must die through the documented
// Stream error contract (channel closed without a StreamDone, reason in Err())
// instead of taking the process with it — what Rust gets from its JoinHandle.
type panickyHook struct{}

func (panickyHook) PreCall(context.Context, ToolCall) error { return nil }
func (panickyHook) PostCall(context.Context, ToolCall, *ToolResult) error {
	panic("post-call hook exploded")
}

func TestRunStreamRecoversPanicOutsideToolExecute(t *testing.T) {
	echo := FuncTool{
		ToolName: "echo",
		Desc:     "Echoes",
		Params:   map[string]any{"type": "object", "properties": map[string]any{}},
		Fn:       func(_ context.Context, _ map[string]any) (string, error) { return "hi", nil },
	}
	mock := NewMockLlmProvider()
	mock.PushResponse(ToolCallResponse("call-1", "echo", `{}`)).
		PushResponse(TextResponse("never reached"))
	agent := NewSmoothAgent(mock, AgentOptions{
		Tools: []Tool{echo},
		Hooks: []ToolHook{panickyHook{}},
	})

	stream, err := agent.RunStream(context.Background(), "use echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, streamErr := drain(stream)
	if streamErr == nil {
		t.Fatal("a panicked turn must be reported through Stream.Err()")
	}
	if !strings.Contains(streamErr.Error(), "panicked") {
		t.Fatalf("Err() should name the panic, got %v", streamErr)
	}
	for _, e := range events {
		if e.Kind == StreamDone {
			t.Fatal("a panicked turn must NOT emit StreamDone")
		}
	}
}
