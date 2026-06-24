package core

import (
	"context"
	"strings"
	"testing"
)

func TestMockReplaysTextInFIFOOrder(t *testing.T) {
	mock := NewMockLlmProvider()
	mock.PushText("first").PushText("second")

	r1, err := mock.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := mock.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Content != "first" || r2.Content != "second" {
		t.Fatalf("FIFO order broken: %q then %q", r1.Content, r2.Content)
	}
}

func TestMockRecordsMessagesAndTools(t *testing.T) {
	mock := NewMockLlmProvider()
	mock.PushText("ok")
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "system", Content: "be helpful"},
			{Role: "user", Content: "hello"},
		},
		Tools: []ToolSpec{{Name: "search", Description: "search", Parameters: map[string]any{}}},
	}
	if _, err := mock.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	if mock.CallCount() != 1 {
		t.Fatalf("want 1 call, got %d", mock.CallCount())
	}
	call, ok := mock.LastCall()
	if !ok {
		t.Fatal("expected a recorded call")
	}
	if len(call.Messages) != 2 || call.Messages[0].Content != "be helpful" || call.Messages[1].Content != "hello" {
		t.Fatalf("messages not recorded: %+v", call.Messages)
	}
	if len(call.Tools) != 1 || call.Tools[0].Name != "search" {
		t.Fatalf("tools not recorded: %+v", call.Tools)
	}
}

func TestMockDefaultWhenScriptEmptyIsBenignTerminal(t *testing.T) {
	mock := NewMockLlmProvider()
	resp, err := mock.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "" || len(resp.ToolCalls) != 0 {
		t.Fatalf("empty script should yield a benign terminal response, got %+v", resp)
	}
}

func TestMockScriptsErrors(t *testing.T) {
	mock := NewMockLlmProvider()
	mock.PushError("rate limited")
	_, err := mock.Chat(context.Background(), ChatRequest{})
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("want rate-limited error, got %v", err)
	}
}

func TestMockToolCallResponseCarriesTheCall(t *testing.T) {
	mock := NewMockLlmProvider()
	mock.PushToolCall("call_1", "get_weather", `{"city": "SF"}`)
	resp, err := mock.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "get_weather" || resp.ToolCalls[0].Arguments != `{"city": "SF"}` {
		t.Fatalf("tool call not carried: %+v", resp.ToolCalls)
	}
}

// MockLlmProvider satisfies the LlmProvider/ChatClient seam at compile time.
var _ LlmProvider = (*MockLlmProvider)(nil)

func TestMockDrivesAFullAgentTurnAndRecordsTheRequest(t *testing.T) {
	echo := FuncTool{
		ToolName: "echo",
		Desc:     "Echoes input back",
		Params:   map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			s, _ := args["text"].(string)
			return s, nil
		},
	}
	mock := NewMockLlmProvider()
	mock.PushToolCall("call-1", "echo", `{"text": "hello tools"}`).PushText("done")

	agent := NewSmoothAgent(mock, AgentOptions{Tools: []Tool{echo}})
	res, err := agent.Run(context.Background(), "use echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" || res.ToolCalls != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	// Two model calls were recorded; the second saw the tool result fed back.
	if mock.CallCount() != 2 {
		t.Fatalf("want 2 recorded calls, got %d", mock.CallCount())
	}
	second := mock.Calls()[1].Messages
	found := false
	for _, m := range second {
		if m.Role == "tool" && m.Content == "hello tools" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool result not fed back; messages=%+v", second)
	}
	// The tool spec was advertised on every call.
	if first := mock.Calls()[0].Tools; len(first) != 1 || first[0].Name != "echo" {
		t.Fatalf("tool spec not advertised: %+v", first)
	}
}
