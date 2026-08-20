package core

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
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

// The cross-language invariant (pearl th-4f1263): a scripted text or tool-call
// response reports 10 prompt / 5 completion tokens in EVERY engine's mock, and a
// drained script still reports nothing. Change these numbers here and the shared
// server scenario corpus goes red.
func TestMockScriptedResponsesReportTheSharedUsageConvention(t *testing.T) {
	mock := NewMockLlmProvider().PushText("hi").PushToolCall("call_1", "search", `{}`)
	for i := 0; i < 2; i++ {
		resp, err := mock.Chat(context.Background(), ChatRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 {
			t.Fatalf("call %d: usage = %+v, want {10 5}", i, resp.Usage)
		}
	}
	// Drained script: "the script ran out" must stay distinguishable from "the
	// model answered", so the fallback reports nothing.
	drained, err := mock.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if drained.Usage != (Usage{}) {
		t.Fatalf("drained script usage = %+v, want zero", drained.Usage)
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

// A multibyte character landing exactly on a chunk boundary must not be split.
//
// Regression for th-6fdd1c: splitIntoChunks sliced by BYTES, so the em-dash in
// "Pro it is — pulling that quote up." (36 bytes, 3 parts ⇒ a boundary at byte 12,
// mid-em-dash) arrived as three U+FFFD. It survived for so long because it is
// invisible with ASCII-only fixtures, and because the sibling scenario's 33-byte
// reply happens to miss every rune boundary.
//
// Each chunk is checked INDIVIDUALLY: concatenating the pieces back together hides
// the corruption in memory, but the wire encodes each chunk on its own.
func TestMockStreamsMultibyteTextWithoutSplittingRunes(t *testing.T) {
	for _, text := range []string{
		"Pro it is — pulling that quote up.", // em-dash, 3 bytes — the original failure
		"ok 🙂 done",                          // emoji, 4 bytes (astral)
		"café münchen 東京",                    // accents + CJK
	} {
		mock := NewMockLlmProvider()
		mock.PushText(text)

		ch, err := mock.ChatStream(context.Background(), ChatRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var got strings.Builder
		for chunk := range ch {
			if chunk.ContentDelta == "" {
				continue
			}
			if !utf8.ValidString(chunk.ContentDelta) {
				t.Errorf("%q: chunk %q is not valid UTF-8 (a rune was split)", text, chunk.ContentDelta)
			}
			got.WriteString(chunk.ContentDelta)
		}
		if got.String() != text {
			t.Errorf("reassembled %q, want %q", got.String(), text)
		}
	}
}

// The same hazard on the tool-call arguments, which are JSON and can carry non-ASCII.
func TestMockStreamsMultibyteToolArgumentsWithoutSplittingRunes(t *testing.T) {
	args := `{"city":"München","reaction":"🙂"}`
	mock := NewMockLlmProvider()
	mock.PushToolCall("call-1", "lookup", args)

	ch, err := mock.ChatStream(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for chunk := range ch {
		for _, d := range chunk.ToolCallDeltas {
			if d.ArgsFragment == "" {
				continue
			}
			if !utf8.ValidString(d.ArgsFragment) {
				t.Errorf("args fragment %q is not valid UTF-8 (a rune was split)", d.ArgsFragment)
			}
			got.WriteString(d.ArgsFragment)
		}
	}
	if got.String() != args {
		t.Errorf("reassembled %q, want %q", got.String(), args)
	}
}
