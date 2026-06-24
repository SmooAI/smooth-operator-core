package core

import (
	"context"
	"encoding/json"
	"math"
	"testing"
)

// drain consumes a stream to completion, returning its events and the terminal error.
func drain(s *Stream) ([]StreamEvent, error) {
	var events []StreamEvent
	for e := range s.Events() {
		events = append(events, e)
	}
	return events, s.Err()
}

func TestRunStreamTextInMultipleChunks(t *testing.T) {
	mock := NewMockLlmProvider().PushResponse(WithUsage(TextResponse("hello there friend, how are you"), 10, 7))
	agent := NewSmoothAgent(mock, AgentOptions{})

	stream, err := agent.RunStream(context.Background(), "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := drain(stream)
	if err != nil {
		t.Fatal(err)
	}

	var text string
	textEvents := 0
	doneEvents := 0
	for _, e := range events {
		switch e.Kind {
		case StreamText:
			text += e.Text
			textEvents++
		case StreamDone:
			doneEvents++
		}
	}
	if textEvents < 2 {
		t.Fatalf("want >=2 text events, got %d", textEvents)
	}
	if text != "hello there friend, how are you" {
		t.Fatalf("concatenated text mismatch: %q", text)
	}
	if doneEvents != 1 {
		t.Fatalf("want exactly 1 done event, got %d", doneEvents)
	}
	if events[len(events)-1].Kind != StreamDone {
		t.Fatalf("done must be last")
	}
	if events[len(events)-1].Response.Text != "hello there friend, how are you" {
		t.Fatalf("done.Response.Text mismatch: %q", events[len(events)-1].Response.Text)
	}
}

func TestRunStreamToolRoundTrip(t *testing.T) {
	var ran string
	echo := FuncTool{
		ToolName: "echo",
		Desc:     "Echoes input",
		Params:   map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			ran, _ = args["text"].(string)
			return "echoed:" + ran, nil
		},
	}
	mock := NewMockLlmProvider()
	mock.PushResponse(WithUsage(ToolCallResponse("call-1", "echo", `{"text":"ping"}`), 5, 3)).
		PushResponse(WithUsage(TextResponse("all done"), 8, 2))
	agent := NewSmoothAgent(mock, AgentOptions{Tools: []Tool{echo}})

	stream, err := agent.RunStream(context.Background(), "use echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := drain(stream)
	if err != nil {
		t.Fatal(err)
	}

	var toolCall, toolResult, done *StreamEvent
	for i := range events {
		switch events[i].Kind {
		case StreamToolCall:
			toolCall = &events[i]
		case StreamToolResult:
			toolResult = &events[i]
		case StreamDone:
			done = &events[i]
		}
	}
	if toolCall == nil || toolCall.Name != "echo" {
		t.Fatalf("missing/incorrect tool_call event: %+v", toolCall)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(toolCall.Arguments), &parsed); err != nil {
		t.Fatalf("tool_call arguments not valid JSON: %v", err)
	}
	if parsed["text"] != "ping" {
		t.Fatalf("tool_call arguments mismatch: %+v", parsed)
	}
	if ran != "ping" {
		t.Fatalf("tool did not run with assembled args; ran=%q", ran)
	}
	if toolResult == nil || toolResult.Result != "echoed:ping" {
		t.Fatalf("missing/incorrect tool_result event: %+v", toolResult)
	}
	if done == nil || done.Response.Text != "all done" || done.Response.Iterations != 2 || done.Response.ToolCalls != 1 {
		t.Fatalf("done response mismatch: %+v", done)
	}
}

func TestRunStreamArgumentsSplitAcrossChunks(t *testing.T) {
	var received map[string]any
	save := FuncTool{
		ToolName: "save",
		Desc:     "Saves",
		Params:   map[string]any{"type": "object"},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			received = args
			return "saved", nil
		},
	}
	mock := NewMockLlmProvider()
	// The mock splits these arguments across two chunks; the agent must reassemble them.
	mock.PushToolCall("call-1", "save", `{"key":"alpha","value":"beta-gamma-delta"}`).PushText("ok")
	agent := NewSmoothAgent(mock, AgentOptions{Tools: []Tool{save}})

	stream, err := agent.RunStream(context.Background(), "save it", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drain(stream); err != nil {
		t.Fatal(err)
	}

	if received["key"] != "alpha" || received["value"] != "beta-gamma-delta" {
		t.Fatalf("arguments not reassembled: %+v", received)
	}
}

func TestRunStreamDoneMatchesRun(t *testing.T) {
	script := func() *MockLlmProvider {
		return NewMockLlmProvider().PushResponse(WithUsage(TextResponse("the answer is 42"), 12, 6))
	}
	opts := AgentOptions{Model: "claude-haiku-4-5"}

	runResult, err := NewSmoothAgent(script(), opts).Run(context.Background(), "q", nil)
	if err != nil {
		t.Fatal(err)
	}

	stream, err := NewSmoothAgent(script(), opts).RunStream(context.Background(), "q", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	var done *StreamEvent
	for i := range events {
		if events[i].Kind == StreamDone {
			done = &events[i]
		}
	}
	if done == nil {
		t.Fatal("no done event")
	}
	r := done.Response
	if r.Text != runResult.Text || r.Iterations != runResult.Iterations || r.ToolCalls != runResult.ToolCalls {
		t.Fatalf("done response != run: stream=%+v run=%+v", r, runResult)
	}
	if r.Usage != runResult.Usage {
		t.Fatalf("usage mismatch: stream=%+v run=%+v", r.Usage, runResult.Usage)
	}
	if math.Abs(r.CostUSD-runResult.CostUSD) > 1e-9 {
		t.Fatalf("cost mismatch: stream=%v run=%v", r.CostUSD, runResult.CostUSD)
	}
	if runResult.CostUSD <= 0 {
		t.Fatalf("expected non-zero cost, got %v", runResult.CostUSD)
	}
}

func TestRunStreamRequiresStreamingClient(t *testing.T) {
	// A ChatClient that does not implement StreamingChatClient.
	agent := NewSmoothAgent(nonStreamingClient{}, AgentOptions{})
	if _, err := agent.RunStream(context.Background(), "hi", nil); err == nil {
		t.Fatal("expected error for non-streaming client")
	}
}

type nonStreamingClient struct{}

func (nonStreamingClient) Chat(_ context.Context, _ ChatRequest) (ChatResponse, error) {
	return TextResponse("x"), nil
}
