package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// --- LLM request metadata (Rust parity: ChatRequest.metadata / with_metadata) ---

func TestMetadataAbsentByDefault(t *testing.T) {
	mock := NewMockLlmProvider().PushText("ok")
	agent := NewSmoothAgent(mock, AgentOptions{})
	if _, err := agent.Run(context.Background(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	last, ok := mock.LastCall()
	if !ok {
		t.Fatal("no recorded call")
	}
	if last.Metadata != nil {
		t.Fatalf("metadata must be nil by default, got %v", last.Metadata)
	}
}

func TestMetadataForwardedVerbatim(t *testing.T) {
	mock := NewMockLlmProvider().PushText("ok")
	meta := map[string]any{"smooai_agent_slug": "support-bot", "k": "v"}
	agent := NewSmoothAgent(mock, AgentOptions{Metadata: meta})
	if _, err := agent.Run(context.Background(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	last, _ := mock.LastCall()
	if last.Metadata["smooai_agent_slug"] != "support-bot" || last.Metadata["k"] != "v" {
		t.Fatalf("metadata must be forwarded verbatim, got %v", last.Metadata)
	}
}

func TestEmptyMetadataNormalizedToNil(t *testing.T) {
	mock := NewMockLlmProvider().PushText("ok")
	agent := NewSmoothAgent(mock, AgentOptions{Metadata: map[string]any{}})
	if _, err := agent.Run(context.Background(), "hi", nil); err != nil {
		t.Fatal(err)
	}
	last, _ := mock.LastCall()
	if last.Metadata != nil {
		t.Fatalf("empty metadata must normalize to nil (wire-identical to unset), got %v", last.Metadata)
	}
}

func TestWireRequestMetadataOmittedAndForwarded(t *testing.T) {
	// nil metadata → no "metadata" key on the wire at all.
	bare, err := json.Marshal(buildWireRequest(ChatRequest{Model: "m"}, false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bare), "metadata") {
		t.Fatalf("unset metadata must be omitted from the wire: %s", bare)
	}
	// empty metadata → normalized away, byte-identical to unset.
	empty, err := json.Marshal(buildWireRequest(ChatRequest{Model: "m", Metadata: map[string]any{}}, false))
	if err != nil {
		t.Fatal(err)
	}
	if string(empty) != string(bare) {
		t.Fatalf("empty metadata must be wire-identical to unset:\n%s\n%s", empty, bare)
	}
	// set metadata → forwarded verbatim as a top-level object.
	tagged, err := json.Marshal(buildWireRequest(ChatRequest{Model: "m", Metadata: map[string]any{"k": "v"}}, false))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(tagged, &parsed); err != nil {
		t.Fatal(err)
	}
	md, ok := parsed["metadata"].(map[string]any)
	if !ok || md["k"] != "v" {
		t.Fatalf("metadata must be forwarded verbatim: %s", tagged)
	}
}

// --- Structured tool details (Rust parity: ToolResult.details → ToolCallComplete.details) ---

// detailsHook attaches structured details in PostCall, the same seam the Rust
// engine populates ToolResult.details through.
type detailsHook struct{ details any }

func (h detailsHook) PreCall(context.Context, ToolCall) error { return nil }
func (h detailsHook) PostCall(_ context.Context, _ ToolCall, result *ToolResult) error {
	result.Details = h.details
	return nil
}

func TestStreamToolResultForwardsDetailsFromHook(t *testing.T) {
	mock := NewMockLlmProvider().
		PushToolCall("call_1", "echo", `{"text":"hi"}`).
		PushText("done")
	details := map[string]any{"traceId": "abc123", "errorCount": 47}
	agent := NewSmoothAgent(mock, AgentOptions{
		Tools: []Tool{FuncTool{
			ToolName: "echo",
			Desc:     "echoes",
			Params:   map[string]any{"type": "object"},
			Fn: func(_ context.Context, args map[string]any) (string, error) {
				s, _ := args["text"].(string)
				return s, nil
			},
		}},
		Hooks: []ToolHook{detailsHook{details: details}},
	})

	stream, err := agent.RunStream(context.Background(), "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Kind == StreamToolResult {
			found = true
			d, ok := e.Details.(map[string]any)
			if !ok || d["traceId"] != "abc123" {
				t.Fatalf("details must be forwarded verbatim on StreamToolResult, got %v", e.Details)
			}
		}
	}
	if !found {
		t.Fatal("expected a StreamToolResult event")
	}
}

func TestStreamToolResultDetailsNilWithoutHook(t *testing.T) {
	mock := NewMockLlmProvider().
		PushToolCall("call_1", "echo", `{"text":"hi"}`).
		PushText("done")
	agent := NewSmoothAgent(mock, AgentOptions{
		Tools: []Tool{FuncTool{
			ToolName: "echo",
			Desc:     "echoes",
			Params:   map[string]any{"type": "object"},
			Fn: func(_ context.Context, args map[string]any) (string, error) {
				s, _ := args["text"].(string)
				return s, nil
			},
		}},
	})

	stream, err := agent.RunStream(context.Background(), "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == StreamToolResult && e.Details != nil {
			t.Fatalf("details must be nil when no hook attaches them, got %v", e.Details)
		}
	}
}
