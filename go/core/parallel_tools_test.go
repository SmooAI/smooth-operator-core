package core

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for concurrent (parallel) tool-call execution. When
// AgentOptions.ParallelToolCalls is true and an assistant turn returns >=2 tool
// calls, dispatches run concurrently (goroutines + sync.WaitGroup) — but the
// tool-result messages must still be appended in the original ToolCalls order so
// the transcript is deterministic. Default (false) keeps sequential dispatch.

// multiToolCall builds an assistant response requesting several tool calls at once.
func multiToolCall(calls ...ToolCall) ChatResponse {
	return ChatResponse{ToolCalls: calls}
}

func toolResults(messages []ChatMessage) []string {
	var out []string
	for _, m := range messages {
		if m.Role == "tool" {
			out = append(out, m.Content)
		}
	}
	return out
}

func TestParallelDispatchOverlaps(t *testing.T) {
	// Two tools that each block until both have started — only completes if concurrent.
	var mu sync.Mutex
	started := 0
	bothStarted := make(chan struct{})
	slow := func(name string) Tool {
		return FuncTool{ToolName: name, Params: map[string]any{"type": "object"},
			Fn: func(ctx context.Context, _ map[string]any) (string, error) {
				mu.Lock()
				started++
				if started == 2 {
					close(bothStarted)
				}
				mu.Unlock()
				<-bothStarted
				return name, nil
			}}
	}
	mock := NewMockLlmProvider()
	mock.PushResponse(multiToolCall(ToolCall{ID: "c1", Name: "a"}, ToolCall{ID: "c2", Name: "b"})).PushText("done")
	agent := NewSmoothAgent(mock, AgentOptions{Tools: []Tool{slow("a"), slow("b")}, ParallelToolCalls: true})

	done := make(chan AgentRunResponse, 1)
	go func() {
		res, err := agent.Run(context.Background(), "go", nil)
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res.Text != "done" || res.ToolCalls != 2 {
			t.Fatalf("unexpected result: %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out — tools did not run concurrently")
	}
}

func TestParallelOrderPreserved(t *testing.T) {
	gates := map[string]chan struct{}{"A": make(chan struct{}), "B": make(chan struct{}), "C": make(chan struct{})}
	make1 := func(name string) Tool {
		return FuncTool{ToolName: name, Params: map[string]any{"type": "object"},
			Fn: func(ctx context.Context, _ map[string]any) (string, error) {
				<-gates[name]
				return "result-" + name, nil
			}}
	}
	mock := NewMockLlmProvider()
	mock.PushResponse(multiToolCall(ToolCall{ID: "c1", Name: "A"}, ToolCall{ID: "c2", Name: "B"}, ToolCall{ID: "c3", Name: "C"})).PushText("done")
	agent := NewSmoothAgent(mock, AgentOptions{Tools: []Tool{make1("A"), make1("B"), make1("C")}, ParallelToolCalls: true})

	done := make(chan struct{})
	go func() {
		if _, err := agent.Run(context.Background(), "go", nil); err != nil {
			t.Error(err)
		}
		close(done)
	}()
	// Finish in B, C, A order — opposite of transcript order for A.
	time.Sleep(5 * time.Millisecond)
	close(gates["B"])
	time.Sleep(5 * time.Millisecond)
	close(gates["C"])
	time.Sleep(5 * time.Millisecond)
	close(gates["A"])
	<-done

	got := toolResults(mock.Calls()[1].Messages)
	want := []string{"result-A", "result-B", "result-C"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("tool results out of order: got %v want %v", got, want)
	}
}

func TestParallelFailingToolKeepsPosition(t *testing.T) {
	ok := func(name string) Tool {
		return FuncTool{ToolName: name, Params: map[string]any{"type": "object"},
			Fn: func(context.Context, map[string]any) (string, error) { return "ok", nil }}
	}
	boom := FuncTool{ToolName: "B", Params: map[string]any{"type": "object"},
		Fn: func(context.Context, map[string]any) (string, error) { return "", &kaboomErr{} }}
	mock := NewMockLlmProvider()
	mock.PushResponse(multiToolCall(ToolCall{ID: "c1", Name: "A"}, ToolCall{ID: "c2", Name: "B"}, ToolCall{ID: "c3", Name: "C"})).PushText("done")
	agent := NewSmoothAgent(mock, AgentOptions{Tools: []Tool{ok("A"), boom, ok("C")}, ParallelToolCalls: true})
	if _, err := agent.Run(context.Background(), "go", nil); err != nil {
		t.Fatal(err)
	}
	got := toolResults(mock.Calls()[1].Messages)
	if len(got) != 3 || got[0] != "ok" || !strings.Contains(got[1], "kaboom") || got[2] != "ok" {
		t.Fatalf("unexpected tool results: %v", got)
	}
}

type kaboomErr struct{}

func (kaboomErr) Error() string { return "kaboom" }

func TestParallelDefaultOffSequential(t *testing.T) {
	var order []string
	var mu sync.Mutex
	make1 := func(name string) Tool {
		return FuncTool{ToolName: name, Params: map[string]any{"type": "object"},
			Fn: func(context.Context, map[string]any) (string, error) {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				return name, nil
			}}
	}
	mock := NewMockLlmProvider()
	mock.PushResponse(multiToolCall(ToolCall{ID: "c1", Name: "A"}, ToolCall{ID: "c2", Name: "B"})).PushText("done")
	agent := NewSmoothAgent(mock, AgentOptions{Tools: []Tool{make1("A"), make1("B")}}) // ParallelToolCalls defaults false
	res, err := agent.Run(context.Background(), "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "A" || order[1] != "B" {
		t.Fatalf("sequential dispatch order wrong: %v", order)
	}
	if res.ToolCalls != 2 {
		t.Fatalf("want 2 tool calls, got %d", res.ToolCalls)
	}
}

func TestParallelSingleToolCallIdentical(t *testing.T) {
	echo := FuncTool{ToolName: "echo", Params: map[string]any{"type": "object"},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			s, _ := args["text"].(string)
			return s, nil
		}}
	mock := NewMockLlmProvider()
	mock.PushToolCall("c1", "echo", `{"text":"hi"}`).PushText("done")
	agent := NewSmoothAgent(mock, AgentOptions{Tools: []Tool{echo}, ParallelToolCalls: true})
	res, err := agent.Run(context.Background(), "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" || res.ToolCalls != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
}
