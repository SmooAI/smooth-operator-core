package core

import (
	"context"
	"errors"
)

// LlmProvider is the LLM-call seam the agent loop depends on. It is the existing
// ChatClient interface under a name that names the role — formalizing the seam so
// the agent loop is unit-testable deterministically, without a live model or
// network. The GatewayClient already implements it; tests inject a MockLlmProvider.
//
// This keeps backward compatibility: NewSmoothAgent still takes a ChatClient, and
// any LlmProvider is a ChatClient (and vice versa) since they are the same shape.
type LlmProvider = ChatClient

// WithUsage returns a copy of resp with the given token usage attached. Handy when
// scripting the mock so the streaming final chunk / non-streaming Usage is non-zero.
func WithUsage(resp ChatResponse, promptTokens, completionTokens int) ChatResponse {
	resp.Usage = Usage{PromptTokens: promptTokens, CompletionTokens: completionTokens}
	return resp
}

// TextResponse builds a plain-text ChatResponse (no tool calls). Handy for
// scripting the mock and for assertions.
func TextResponse(content string) ChatResponse {
	return ChatResponse{Content: content}
}

// ToolCallResponse builds a ChatResponse that requests a single tool call.
// arguments is the raw JSON-string the model emits for the call's arguments.
func ToolCallResponse(id, name, arguments string) ChatResponse {
	return ChatResponse{ToolCalls: []ToolCall{{ID: id, Name: name, Arguments: arguments}}}
}

// scriptedOutcome is one entry in the mock's script: a response or an error.
type scriptedOutcome struct {
	resp ChatResponse
	err  error
}

// MockLlmProvider is a deterministic LlmProvider for tests. Script the responses
// it should return (FIFO), drive your code, then assert on Calls. Build it up
// fluently with PushText / PushToolCall / PushError.
//
// It replaces the ad-hoc fakeClient the tests rolled by hand, and mirrors the
// BEHAVIOR of the Rust reference's MockLlmClient (record + replay). It is not
// safe for concurrent use — a turn drives it serially, which is the intended use.
type MockLlmProvider struct {
	script []scriptedOutcome
	calls  []ChatRequest
}

// NewMockLlmProvider returns an empty mock. Script it with the Push* methods.
func NewMockLlmProvider() *MockLlmProvider {
	return &MockLlmProvider{}
}

// PushResponse queues a raw ChatResponse for the next Chat call.
func (m *MockLlmProvider) PushResponse(resp ChatResponse) *MockLlmProvider {
	m.script = append(m.script, scriptedOutcome{resp: resp})
	return m
}

// PushText queues a plain-text response for the next Chat call.
func (m *MockLlmProvider) PushText(content string) *MockLlmProvider {
	return m.PushResponse(TextResponse(content))
}

// PushToolCall queues a single-tool-call response for the next Chat call.
func (m *MockLlmProvider) PushToolCall(id, name, arguments string) *MockLlmProvider {
	return m.PushResponse(ToolCallResponse(id, name, arguments))
}

// PushError queues an error to be returned from the next Chat call.
func (m *MockLlmProvider) PushError(message string) *MockLlmProvider {
	m.script = append(m.script, scriptedOutcome{err: errors.New(message)})
	return m
}

// Calls returns every request the mock has received so far, in order.
func (m *MockLlmProvider) Calls() []ChatRequest {
	return m.calls
}

// CallCount returns the number of requests received.
func (m *MockLlmProvider) CallCount() int {
	return len(m.calls)
}

// LastCall returns the most recent request and true, or a zero request and false
// if none have been received.
func (m *MockLlmProvider) LastCall() (ChatRequest, bool) {
	if len(m.calls) == 0 {
		return ChatRequest{}, false
	}
	return m.calls[len(m.calls)-1], true
}

// Chat implements ChatClient / LlmProvider: it records the request, then replays
// the next scripted outcome. With an empty script it returns a benign empty text
// response so loops terminate cleanly.
func (m *MockLlmProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	m.calls = append(m.calls, req)
	next, ok := m.pop()
	if !ok {
		return ChatResponse{}, nil
	}
	if next.err != nil {
		return ChatResponse{}, next.err
	}
	return next.resp, nil
}

// ChatStream implements StreamingChatClient: it records the request, then replays
// the SAME next scripted outcome as Chat, but as chunked deltas. Text is split into
// a few content-delta chunks; each tool call is split into an opening chunk (ID +
// name + first half of arguments) and a second chunk with the rest of the arguments
// (exercising the agent's index-keyed accumulator); a final chunk carries usage. A
// scripted error is returned synchronously (connect-time), mirroring a failed open.
func (m *MockLlmProvider) ChatStream(_ context.Context, req ChatRequest) (<-chan ChatChunk, error) {
	m.calls = append(m.calls, req)
	next, ok := m.pop()
	if !ok {
		next = scriptedOutcome{resp: TextResponse("")}
	}
	if next.err != nil {
		return nil, next.err
	}
	ch := make(chan ChatChunk)
	resp := next.resp
	go func() {
		defer close(ch)
		for _, piece := range splitIntoChunks(resp.Content, 3) {
			ch <- ChatChunk{ContentDelta: piece}
		}
		for i, tc := range resp.ToolCalls {
			mid := len(tc.Arguments) / 2
			ch <- ChatChunk{ToolCallDeltas: []ToolCallDelta{{Index: i, ID: tc.ID, Name: tc.Name, ArgsFragment: tc.Arguments[:mid]}}}
			ch <- ChatChunk{ToolCallDeltas: []ToolCallDelta{{Index: i, ArgsFragment: tc.Arguments[mid:]}}}
		}
		u := resp.Usage
		ch <- ChatChunk{Usage: &u}
	}()
	return ch, nil
}

// pop removes and returns the next scripted outcome, or false if the script is empty.
func (m *MockLlmProvider) pop() (scriptedOutcome, bool) {
	if len(m.script) == 0 {
		return scriptedOutcome{}, false
	}
	next := m.script[0]
	m.script = m.script[1:]
	return next, true
}

// splitIntoChunks splits s into up to n roughly-equal non-empty pieces.
func splitIntoChunks(s string, n int) []string {
	if s == "" {
		return nil
	}
	parts := n
	if len(s) < parts {
		parts = len(s)
	}
	if parts < 1 {
		parts = 1
	}
	size := (len(s) + parts - 1) / parts // ceil
	var out []string
	for i := 0; i < len(s); i += size {
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[i:end])
	}
	return out
}
