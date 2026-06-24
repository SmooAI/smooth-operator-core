package core

import (
	"context"
	"testing"
)

func TestFreshThreadHasIDAndNoMessages(t *testing.T) {
	th := NewThread()
	if th.ID == "" {
		t.Fatal("fresh thread should have an auto-generated id")
	}
	if th.Len() != 0 || len(th.Messages()) != 0 {
		t.Fatalf("fresh thread should have no messages, got %d", th.Len())
	}
	// Two fresh threads get distinct ids.
	if NewThread().ID == NewThread().ID {
		t.Fatal("fresh threads should have distinct ids")
	}
}

func TestThreadResumesWithExplicitID(t *testing.T) {
	if NewThreadWithID("conv-42").ID != "conv-42" {
		t.Fatal("explicit id not honored")
	}
	// Empty id falls back to a fresh generated one.
	if NewThreadWithID("").ID == "" {
		t.Fatal("empty id should fall back to a generated id")
	}
}

func TestThreadNeverStoresSystemMessages(t *testing.T) {
	th := NewThread()
	th.Add(ChatMessage{Role: "system", Content: "you are helpful"})
	th.Add(ChatMessage{Role: "user", Content: "hi"})
	th.Extend([]ChatMessage{
		{Role: "system", Content: "ignored"},
		{Role: "assistant", Content: "hello"},
	})
	roles := rolesOf(th.Messages())
	if len(roles) != 2 || roles[0] != "user" || roles[1] != "assistant" {
		t.Fatalf("system messages should be skipped, got %v", roles)
	}
}

func TestThreadCarriesHistoryAcrossRuns(t *testing.T) {
	client := &fakeClient{scripted: []ChatResponse{{Content: "first answer"}, {Content: "second answer"}}}
	agent := NewSmoothAgent(client, AgentOptions{Instructions: "be helpful"})
	th := NewThread()

	// Turn 1 — seeds nothing prior; appends [user, assistant] to the thread.
	if _, err := agent.RunThread(context.Background(), "hello", th); err != nil {
		t.Fatal(err)
	}
	roles := rolesOf(th.Messages())
	if len(roles) != 2 || roles[0] != "user" || roles[1] != "assistant" {
		t.Fatalf("after turn 1 want [user assistant], got %v", roles)
	}
	if th.Messages()[0].Content != "hello" || th.Messages()[1].Content != "first answer" {
		t.Fatalf("unexpected turn-1 contents: %+v", th.Messages())
	}

	// Turn 2 — the second model call must see turn 1's history.
	if _, err := agent.RunThread(context.Background(), "again", th); err != nil {
		t.Fatal(err)
	}
	second := client.calls[1].Messages
	var sawHello, sawFirst, sawAgain, sawSystem bool
	for _, m := range second {
		switch {
		case m.Role == "system":
			sawSystem = true
		case m.Content == "hello":
			sawHello = true
		case m.Content == "first answer":
			sawFirst = true
		case m.Content == "again":
			sawAgain = true
		}
	}
	if !sawHello || !sawFirst || !sawAgain {
		t.Fatalf("second call missing prior history: %+v", second)
	}
	if !sawSystem {
		t.Fatal("system prompt should be rebuilt per-run and present in the call")
	}

	// The thread holds the full 4-message conversation, no system message.
	if got := rolesOf(th.Messages()); len(got) != 4 {
		t.Fatalf("expected 4 accumulated messages, got %d (%v)", len(got), got)
	}
	for _, m := range th.Messages() {
		if m.Role == "system" {
			t.Fatal("thread must never store a system message")
		}
	}
}

func TestThreadSeedsNoPriorOnFirstRun(t *testing.T) {
	client := &fakeClient{scripted: []ChatResponse{{Content: "hi there"}}}
	agent := NewSmoothAgent(client, AgentOptions{Instructions: "be helpful"})
	th := NewThread()

	if _, err := agent.RunThread(context.Background(), "hello", th); err != nil {
		t.Fatal(err)
	}
	// The only model call: system + the single user message, nothing prior.
	first := client.calls[0].Messages
	if got := rolesOf(first); len(got) != 2 || got[0] != "system" || got[1] != "user" {
		t.Fatalf("first call should be [system user], got %v", got)
	}
}

func TestThreadAccumulatesToolMessages(t *testing.T) {
	echo := FuncTool{
		ToolName: "echo",
		Desc:     "echo",
		Params:   map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			s, _ := args["text"].(string)
			return s, nil
		},
	}
	client := &fakeClient{scripted: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "call-1", Name: "echo", Arguments: `{"text": "hi"}`}}},
		{Content: "done"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{Tools: []Tool{echo}})
	th := NewThread()

	if _, err := agent.RunThread(context.Background(), "please echo", th); err != nil {
		t.Fatal(err)
	}
	// user, assistant(tool_call), tool result, assistant(final answer)
	want := []string{"user", "assistant", "tool", "assistant"}
	got := rolesOf(th.Messages())
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}

func TestSingleShotRunStillWorksWithoutThread(t *testing.T) {
	client := &fakeClient{scripted: []ChatResponse{{Content: "the answer is 42"}}}
	agent := NewSmoothAgent(client, AgentOptions{Instructions: "be helpful"})
	res, err := agent.Run(context.Background(), "what is the answer?", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "the answer is 42" || res.Iterations != 1 || res.ToolCalls != 0 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func rolesOf(msgs []ChatMessage) []string {
	roles := make([]string, len(msgs))
	for i, m := range msgs {
		roles[i] = m.Role
	}
	return roles
}
