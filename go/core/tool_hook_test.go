package core

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// spyHook records every PreCall/PostCall it sees, so a test can assert both
// lifecycle points fire for a dispatched tool.
type spyHook struct {
	pre  []string
	post []string
}

func (h *spyHook) PreCall(_ context.Context, call ToolCall) error {
	h.pre = append(h.pre, call.Name)
	return nil
}

func (h *spyHook) PostCall(_ context.Context, call ToolCall, _ *ToolResult) error {
	h.post = append(h.post, call.Name)
	return nil
}

// redactHook rewrites the tool result content in place (the redaction seam).
type redactHook struct{}

func (redactHook) PreCall(_ context.Context, _ ToolCall) error { return nil }
func (redactHook) PostCall(_ context.Context, _ ToolCall, result *ToolResult) error {
	result.Content = strings.ReplaceAll(result.Content, "secret", "[REDACTED]")
	return nil
}

// blockHook denies a named tool from PreCall.
type blockHook struct{ blocked string }

func (b blockHook) PreCall(_ context.Context, call ToolCall) error {
	if call.Name == b.blocked {
		return errors.New("blocked by policy")
	}
	return nil
}
func (blockHook) PostCall(_ context.Context, _ ToolCall, _ *ToolResult) error { return nil }

func echoTool() FuncTool {
	return FuncTool{
		ToolName: "echo",
		Desc:     "Echoes text back.",
		Params:   map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			t, _ := args["text"].(string)
			return t, nil
		},
	}
}

func TestHookFiresPreAndPost(t *testing.T) {
	hook := &spyHook{}
	client := &fakeClient{scripted: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "echo", Arguments: `{"text":"hi"}`}}},
		{Content: "done"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{
		Tools: []Tool{echoTool()},
		Hooks: []ToolHook{hook},
	})

	res, err := agent.Run(context.Background(), "say hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" {
		t.Fatalf("unexpected final text: %q", res.Text)
	}
	if len(hook.pre) != 1 || hook.pre[0] != "echo" {
		t.Fatalf("PreCall should fire once for echo; got %v", hook.pre)
	}
	if len(hook.post) != 1 || hook.post[0] != "echo" {
		t.Fatalf("PostCall should fire once for echo; got %v", hook.post)
	}
}

func TestPostCallRedactsResult(t *testing.T) {
	client := &fakeClient{scripted: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "echo", Arguments: `{"text":"the secret is here"}`}}},
		{Content: "ok"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{
		Tools: []Tool{echoTool()},
		Hooks: []ToolHook{redactHook{}},
	})

	if _, err := agent.Run(context.Background(), "echo", nil); err != nil {
		t.Fatal(err)
	}
	// The redacted content — not the raw tool output — must reach the model.
	second := client.calls[1].Messages
	var toolMsg string
	for _, m := range second {
		if m.Role == "tool" {
			toolMsg = m.Content
		}
	}
	if strings.Contains(toolMsg, "secret") || !strings.Contains(toolMsg, "[REDACTED]") {
		t.Fatalf("PostCall redaction must reach the model; got %q", toolMsg)
	}
}

func TestPreCallErrorBlocksTool(t *testing.T) {
	var ran bool
	tool := FuncTool{
		ToolName: "echo",
		Desc:     "Echoes.",
		Params:   map[string]any{"type": "object"},
		Fn: func(_ context.Context, _ map[string]any) (string, error) {
			ran = true
			return "should not run", nil
		},
	}
	client := &fakeClient{scripted: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "echo", Arguments: `{}`}}},
		{Content: "understood"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{
		Tools: []Tool{tool},
		Hooks: []ToolHook{blockHook{blocked: "echo"}},
	})

	if _, err := agent.Run(context.Background(), "echo", nil); err != nil {
		t.Fatal(err)
	}
	if ran {
		t.Fatal("blocked tool must not execute")
	}
	// The block reason is fed back to the model as the tool result.
	second := client.calls[1].Messages
	var toolMsg string
	for _, m := range second {
		if m.Role == "tool" {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "blocked by hook") || !strings.Contains(toolMsg, "blocked by policy") {
		t.Fatalf("block reason not fed back; got %q", toolMsg)
	}
}
