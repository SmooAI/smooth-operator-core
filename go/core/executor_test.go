package core

import (
	"context"
	"testing"
)

// echoTool (tool_hook_test.go) is the Go sibling of the Rust EchoTool these
// tests mirror.
func seedConversation(user string) []ChatMessage {
	return []ChatMessage{
		{Role: "system", Content: "You are a test agent"},
		{Role: "user", Content: user},
	}
}

// The in-process executor drives the loop identically to SmoothAgent.Run: a
// single text response with no tool calls ends the turn after one model call,
// with the assistant content the mock scripted.
func TestInProcessExecutorMatchesAgentRun(t *testing.T) {
	mock := NewMockLlmProvider().PushText("the answer is 42")
	agent := NewSmoothAgent(mock, AgentOptions{Instructions: "be helpful"})

	res, err := NewInProcessExecutor().Execute(context.Background(), agent, "what is the answer?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "the answer is 42" || res.Iterations != 1 || res.ToolCalls != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("want 1 model call, got %d", mock.CallCount())
	}
	found := false
	for _, m := range mock.Calls()[0].Messages {
		if m.Role == "user" && m.Content == "what is the answer?" {
			found = true
		}
	}
	if !found {
		t.Fatalf("user message not sent; messages=%+v", mock.Calls()[0].Messages)
	}
}

// The streaming entry point surfaces events and returns the same final result.
func TestInProcessExecutorStreamingEmitsEventsAndReturnsResult(t *testing.T) {
	mock := NewMockLlmProvider().PushText("streamed reply")
	agent := NewSmoothAgent(mock, AgentOptions{Instructions: "be helpful"})

	stream, err := NewInProcessExecutor().ExecuteStreaming(context.Background(), agent, "stream please", nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []StreamEvent
	var final AgentRunResponse
	for ev := range stream.Events() {
		events = append(events, ev)
		if ev.Kind == StreamDone {
			final = ev.Response
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("expected streaming events to be emitted")
	}
	if final.Text != "streamed reply" || final.Iterations != 1 {
		t.Fatalf("unexpected final response: %+v", final)
	}
}

// The executor is usable through the interface, which is what lets a consumer
// hold an AgentExecutor and swap backends (Go's analogue of Rust's
// object-safety test).
func TestExecutorIsUsableThroughInterface(t *testing.T) {
	mock := NewMockLlmProvider().PushText("dyn ok")
	agent := NewSmoothAgent(mock, AgentOptions{})

	var executor AgentExecutor = NewInProcessExecutor()
	res, err := executor.Execute(context.Background(), agent, "via interface", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "dyn ok" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

// A plain text reply ends the turn after exactly one model call, with the
// assistant content the mock scripted — no tools run.
func TestDriveTurnTextReplyStopsAfterOneModelCall(t *testing.T) {
	mock := NewMockLlmProvider().PushText("the answer is 42")
	activities := NewInProcessActivities(mock, "test-model", nil)

	convo, err := DriveTurn(context.Background(), activities, seedConversation("what is the answer?"), nil, DefaultTurnPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("want 1 model call, got %d", mock.CallCount())
	}
	last := convo[len(convo)-1]
	if last.Role != "assistant" || last.Content != "the answer is 42" {
		t.Fatalf("unexpected tail message: %+v", last)
	}
}

// A tool call is executed and its result appended paired to the call, then the
// follow-up text reply ends the turn — two model calls, one tool result between.
func TestDriveTurnRunsToolThenFinishes(t *testing.T) {
	mock := NewMockLlmProvider()
	mock.PushToolCall("call-1", "echo", `{"text": "hello tools"}`).PushText("done")
	activities := NewInProcessActivities(mock, "test-model", []Tool{echoTool()})

	convo, err := DriveTurn(context.Background(), activities, seedConversation("use the echo tool"), nil, DefaultTurnPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if mock.CallCount() != 2 {
		t.Fatalf("want 2 model calls, got %d", mock.CallCount())
	}
	var toolMsgs []ChatMessage
	for _, m := range convo {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 1 {
		t.Fatalf("want 1 tool message, got %d (%+v)", len(toolMsgs), convo)
	}
	if toolMsgs[0].Content != "hello tools" || toolMsgs[0].ToolCallID != "call-1" {
		t.Fatalf("tool result not paired to the call: %+v", toolMsgs[0])
	}
	// The tool result was fed back to the model on the second call.
	fed := false
	for _, m := range mock.Calls()[1].Messages {
		if m.Role == "tool" && m.Content == "hello tools" {
			fed = true
		}
	}
	if !fed {
		t.Fatalf("tool result not fed back; messages=%+v", mock.Calls()[1].Messages)
	}
	last := convo[len(convo)-1]
	if last.Role != "assistant" || last.Content != "done" {
		t.Fatalf("unexpected tail message: %+v", last)
	}
}

// MaxIterations bounds the loop: a model that keeps requesting tools stops after
// exactly that many model calls, returning what it has rather than an error.
func TestDriveTurnMaxIterationsBoundsLoop(t *testing.T) {
	mock := NewMockLlmProvider()
	for i := 0; i < 5; i++ {
		mock.PushToolCall("call-1", "echo", `{"text": "again"}`)
	}
	activities := NewInProcessActivities(mock, "test-model", []Tool{echoTool()})

	convo, err := DriveTurn(context.Background(), activities, seedConversation("loop forever"), nil, TurnPolicy{MaxIterations: 2})
	if err != nil {
		t.Fatal(err)
	}
	if mock.CallCount() != 2 {
		t.Fatalf("want the loop bounded at 2 model calls, got %d", mock.CallCount())
	}
	// 2 seed + 2 * (assistant + tool result).
	if len(convo) != 6 {
		t.Fatalf("want 6 messages after 2 bounded iterations, got %d (%+v)", len(convo), convo)
	}
}
