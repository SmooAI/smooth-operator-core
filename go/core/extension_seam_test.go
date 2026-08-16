package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The SEP seam, driven end-to-end through the real agent loop with a scripted
// client. These cover the WIRING — that the host is actually reached from the
// loop — which is exactly what was missing: the extension host was published but
// unreachable, with zero references from the agent.
//
// The host itself (fold policy, cross-tool guard, subprocess) is tested in
// go/core/extension; a fake ExtensionHooks keeps these tests deterministic and
// subprocess-free.

// fakeHooks is a scripted ExtensionHooks that records what the loop asked it.
type fakeHooks struct {
	tools         []Tool
	deferred      []Tool
	blockTool     string // veto any call to this tool
	blockReason   string
	patchArgsFor  string // rewrite arguments for this tool
	patchArgs     string
	events        []string
	eventPayloads map[string]string
	hookedTools   []string
}

func (f *fakeHooks) ExtensionTools() []Tool         { return f.tools }
func (f *fakeHooks) ExtensionDeferredTools() []Tool { return f.deferred }

func (f *fakeHooks) RunToolCallHook(_ context.Context, tool string, arguments string) (bool, string, string) {
	f.hookedTools = append(f.hookedTools, tool)
	if tool == f.blockTool {
		return true, f.blockReason, ""
	}
	if tool == f.patchArgsFor {
		return false, "", f.patchArgs
	}
	return false, "", ""
}

func (f *fakeHooks) DispatchEvent(event string, payload json.RawMessage) {
	f.events = append(f.events, event)
	if f.eventPayloads == nil {
		f.eventPayloads = map[string]string{}
	}
	f.eventPayloads[event] = string(payload)
}

// recordingTool captures the arguments it was executed with.
type recordingTool struct {
	name    string
	gotArgs map[string]any
	calls   int
}

func (t *recordingTool) Name() string               { return t.name }
func (t *recordingTool) Description() string        { return "records its arguments" }
func (t *recordingTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (t *recordingTool) Execute(_ context.Context, args map[string]any) (string, error) {
	t.calls++
	t.gotArgs = args
	return "ok", nil
}

func toolCallResponse(id, name, args string) ChatResponse {
	return ChatResponse{ToolCalls: []ToolCall{{ID: id, Name: name, Arguments: args}}}
}

func TestExtensionToolsAreVisibleAndDispatchable(t *testing.T) {
	ext := &recordingTool{name: "weather.forecast"}
	hooks := &fakeHooks{tools: []Tool{ext}}
	client := &fakeClient{scripted: []ChatResponse{
		toolCallResponse("c1", "weather.forecast", `{"city":"NYC"}`),
		{Content: "done"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{Extensions: hooks})

	if _, err := agent.Run(context.Background(), "weather?", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Visible to the model as an ordinary tool...
	specs := client.calls[0].Tools
	if len(specs) != 1 || specs[0].Name != "weather.forecast" {
		t.Fatalf("extension tool not offered to the model: %+v", specs)
	}
	// ...and actually executed when called.
	if ext.calls != 1 {
		t.Errorf("extension tool executed %d times, want 1", ext.calls)
	}
}

func TestExtensionDeferredToolsStayHiddenUntilPromoted(t *testing.T) {
	// The deferred-tool invariant must survive the merge: an unpromoted deferred
	// tool is never offered to the model.
	hidden := &recordingTool{name: "vault.read"}
	hooks := &fakeHooks{deferred: []Tool{hidden}}
	client := &fakeClient{scripted: []ChatResponse{{Content: "hi"}}}
	agent := NewSmoothAgent(client, AgentOptions{Extensions: hooks})

	if _, err := agent.Run(context.Background(), "hello", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, spec := range client.calls[0].Tools {
		if spec.Name == "vault.read" {
			t.Fatal("an unpromoted deferred extension tool must not be offered to the model")
		}
	}
}

func TestToolCallHookVetoBlocksExecution(t *testing.T) {
	native := &recordingTool{name: "bash"}
	hooks := &fakeHooks{blockTool: "bash", blockReason: "policy: no shell"}
	client := &fakeClient{scripted: []ChatResponse{
		toolCallResponse("c1", "bash", `{"command":"rm -rf /"}`),
		{Content: "understood"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{Tools: []Tool{native}, Extensions: hooks})

	if _, err := agent.Run(context.Background(), "clean up", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if native.calls != 0 {
		t.Error("a vetoed tool call must never execute")
	}
	// The model is told why, rather than seeing an empty result.
	var toolMsg string
	for _, m := range client.calls[1].Messages {
		if m.Role == "tool" {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "policy: no shell") {
		t.Errorf("veto reason not surfaced to the model: %q", toolMsg)
	}
}

func TestToolCallHookModifyRewritesArguments(t *testing.T) {
	ext := &recordingTool{name: "weather.forecast"}
	hooks := &fakeHooks{
		tools:        []Tool{ext},
		patchArgsFor: "weather.forecast",
		patchArgs:    `{"city":"Boston"}`,
	}
	client := &fakeClient{scripted: []ChatResponse{
		toolCallResponse("c1", "weather.forecast", `{"city":"NYC"}`),
		{Content: "done"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{Extensions: hooks})

	if _, err := agent.Run(context.Background(), "weather?", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := ext.gotArgs["city"]; got != "Boston" {
		t.Errorf("arguments not rewritten by the hook: city = %v, want Boston", got)
	}
}

func TestTurnEventsAreDispatched(t *testing.T) {
	hooks := &fakeHooks{}
	client := &fakeClient{scripted: []ChatResponse{{Content: "hello there"}}}
	agent := NewSmoothAgent(client, AgentOptions{Extensions: hooks})

	if _, err := agent.Run(context.Background(), "hi", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{sepTurnStart, sepMessageEnd, sepTurnEnd}
	if len(hooks.events) != len(want) {
		t.Fatalf("events = %v, want %v", hooks.events, want)
	}
	for i, e := range want {
		if hooks.events[i] != e {
			t.Fatalf("events = %v, want %v", hooks.events, want)
		}
	}
	// message_end carries the assistant text the extension is meant to observe.
	if !strings.Contains(hooks.eventPayloads[sepMessageEnd], "hello there") {
		t.Errorf("message_end payload = %s", hooks.eventPayloads[sepMessageEnd])
	}
}

func TestNoExtensionHostLeavesTheLoopUnchanged(t *testing.T) {
	// The zero-cost default: nil Extensions means no hook calls and no dispatch.
	native := &recordingTool{name: "echo"}
	client := &fakeClient{scripted: []ChatResponse{
		toolCallResponse("c1", "echo", `{"x":1}`),
		{Content: "done"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{Tools: []Tool{native}})

	if _, err := agent.Run(context.Background(), "go", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if native.calls != 1 {
		t.Errorf("tool executed %d times, want 1", native.calls)
	}
}
