package temporal

import (
	"encoding/json"
	"testing"

	core "github.com/SmooAI/smooth-operator-core/go/core"
)

// These tests exercise the serde boundary the durable path marshals across — the
// activity inputs and the model-call output — and run WITHOUT a Temporal server (no
// SDK runtime, just JSON), mirroring the Rust `dto.rs` unit tests that are always
// compiled regardless of the `temporal` feature.

// ModelCallInput survives a JSON round trip: the context window and tool specs cross
// the activity boundary intact.
func TestModelCallInput_SerdeRoundTrip(t *testing.T) {
	in := ModelCallInput{
		Messages: []core.ChatMessage{
			{Role: "system", Content: "you are a test agent"},
			{Role: "user", Content: "what is the durable answer?"},
		},
		Tools: []core.ToolSpec{
			{Name: "echo", Description: "echo it back", Parameters: map[string]any{"type": "object"}},
		},
	}
	blob, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ModelCallInput
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.Messages) != 2 || back.Messages[1].Content != "what is the durable answer?" {
		t.Fatalf("messages not preserved: %+v", back.Messages)
	}
	if len(back.Tools) != 1 || back.Tools[0].Name != "echo" {
		t.Fatalf("tools not preserved: %+v", back.Tools)
	}
}

// ToolInvokeInput survives a JSON round trip: the tool call (id + name + raw-JSON
// arguments) crosses the activity boundary intact.
func TestToolInvokeInput_SerdeRoundTrip(t *testing.T) {
	in := ToolInvokeInput{Call: core.ToolCall{ID: "call-1", Name: "echo", Arguments: `{"text":"hi"}`}}
	blob, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ToolInvokeInput
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Call.ID != "call-1" || back.Call.Name != "echo" || back.Call.Arguments != `{"text":"hi"}` {
		t.Fatalf("call not preserved: %+v", back.Call)
	}
}

// The model-call activity output — core.ChatResponse used directly as the DTO —
// survives a JSON round trip preserving every field the orchestration and
// accounting read (content, tool calls, usage, gateway cost). This is the Go analog
// of the Rust `model_call_output_round_trips_through_llm_response` test; Go needs no
// separate projection type because ChatResponse is already fully serializable.
func TestChatResponse_ActivityOutputRoundTrip(t *testing.T) {
	cost := 0.0001
	original := core.ChatResponse{
		Content: "the durable answer is 42",
		ToolCalls: []core.ToolCall{
			{ID: "call-1", Name: "echo", Arguments: `{"text":"hi"}`},
		},
		Usage:          core.Usage{PromptTokens: 10, CompletionTokens: 5},
		GatewayCostUSD: &cost,
	}
	blob, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back core.ChatResponse
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Content != original.Content {
		t.Errorf("content: got %q", back.Content)
	}
	if len(back.ToolCalls) != 1 || back.ToolCalls[0].ID != "call-1" {
		t.Errorf("tool calls not preserved: %+v", back.ToolCalls)
	}
	if back.Usage.TotalTokens() != 15 {
		t.Errorf("usage total: got %d", back.Usage.TotalTokens())
	}
	if back.GatewayCostUSD == nil || *back.GatewayCostUSD != cost {
		t.Errorf("gateway cost not preserved: %v", back.GatewayCostUSD)
	}
}

// parseWaitSeconds accepts a whole non-negative integer and rejects everything else
// (missing, non-numeric, negative, fractional, unparseable) — the guard the durable
// wait tool relies on to fail cleanly instead of sleeping on garbage.
func TestParseWaitSeconds(t *testing.T) {
	cases := []struct {
		args   string
		want   int64
		wantOK bool
	}{
		{`{"seconds":1}`, 1, true},
		{`{"seconds":0}`, 0, true},
		{`{"seconds":86400}`, 86400, true},
		{`{}`, 0, false},
		{``, 0, false},
		{`{"seconds":"5"}`, 0, false},
		{`{"seconds":-3}`, 0, false},
		{`{"seconds":1.5}`, 0, false},
		{`not json`, 0, false},
	}
	for _, c := range cases {
		got, ok := parseWaitSeconds(c.args)
		if ok != c.wantOK || got != c.want {
			t.Errorf("parseWaitSeconds(%q) = (%d, %v), want (%d, %v)", c.args, got, ok, c.want, c.wantOK)
		}
	}
}
