package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These cover the three ways the Go streaming path used to lose data or resources
// SILENTLY, all of which the Rust reference engine already handled:
//
//  1. a stream torn mid-flight (idle stall, overall timeout, reset connection)
//     ended the scan loop, closed the channel, and produced a StreamDone carrying
//     HALF an answer with Stream.Err() == nil — the partial text then went into
//     history and the checkpoint as the assistant turn;
//  2. runStream ran no SEP hooks, so an extension's tool_call veto applied on Run
//     and not on RunStream — i.e. enforced nothing;
//  3. every send was a bare channel send onto an unbuffered channel, so a consumer
//     that stopped ranging early parked the turn goroutine forever, still holding
//     its socket.

// sseHandler writes an SSE preamble and hands the flusher to fn.
func sseHandler(fn func(w http.ResponseWriter, r *http.Request, flush func())) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flush := func() {
			if flusher != nil {
				flusher.Flush()
			}
		}
		flush()
		fn(w, r, flush)
	}
}

// noGoroutineMatching polls the full goroutine dump until no stack mentions frame,
// which is a far tighter leak assertion than a goroutine COUNT: a count tolerance
// wide enough to absorb httptest's connection goroutines is also wide enough to
// hide the one wedged turn goroutine under test.
func noGoroutineMatching(t *testing.T, frame string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		dump := string(buf[:runtime.Stack(buf, true)])
		if !strings.Contains(dump, frame) {
			return
		}
		if time.Now().After(deadline) {
			for _, g := range strings.Split(dump, "\n\n") {
				if strings.Contains(g, frame) {
					t.Fatalf("goroutine leaked (still in %s):\n%s", frame, g)
				}
			}
			t.Fatalf("goroutine leaked (still in %s)", frame)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func sseDelta(text string) string {
	return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", text)
}

// TestStreamTornMidFlightIsAnErrorNotACleanFinish is the core regression: the
// server sends part of an answer and then rips the connection down without
// terminating the chunked body. The turn MUST abort — no StreamDone, non-nil
// Err() — instead of reporting the fragment as a finished assistant turn.
func TestStreamTornMidFlightIsAnErrorNotACleanFinish(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		writeChunk := func(s string) { fmt.Fprintf(buf, "%x\r\n%s\r\n", len(s), s) }
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		writeChunk(sseDelta("half a sen"))
		buf.Flush()
		// No terminating zero-length chunk: the body is TORN, exactly as a
		// mid-stream reset or an expired client timeout leaves it.
		conn.Close()
	}))
	defer srv.Close()

	agent := NewSmoothAgent(NewGatewayClient(srv.URL, "k"), AgentOptions{})
	stream, err := agent.RunStream(context.Background(), "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := drain(stream)
	if err == nil {
		t.Fatal("truncated stream reported as a clean turn: Err() == nil")
	}
	if !strings.Contains(err.Error(), "stream") {
		t.Fatalf("error should name the stream failure, got %v", err)
	}
	for _, e := range events {
		if e.Kind == StreamDone {
			t.Fatalf("truncated stream emitted StreamDone with %q", e.Response.Text)
		}
	}
}

// TestStreamTruncationDoesNotCheckpointPartialText: the half-answer must not be
// persisted as the assistant turn. This is what made the bug invisible — the next
// turn read the fragment back as if the model had said it.
func TestStreamTruncationDoesNotCheckpointPartialText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, _ := w.(http.Hijacker).Hijack()
		writeChunk := func(s string) { fmt.Fprintf(buf, "%x\r\n%s\r\n", len(s), s) }
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		writeChunk(sseDelta("half a sen"))
		buf.Flush()
		conn.Close()
	}))
	defer srv.Close()

	thread := NewThread()
	agent := NewSmoothAgent(NewGatewayClient(srv.URL, "k"), AgentOptions{})
	stream, err := agent.RunStream(context.Background(), "hi", thread)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drain(stream); err == nil {
		t.Fatal("truncated stream reported as a clean turn")
	}
	for _, m := range thread.Messages() {
		if m.Role == "assistant" && strings.Contains(m.Content, "half a sen") {
			t.Fatalf("truncated assistant text was persisted to the thread: %q", m.Content)
		}
	}
}

// TestStreamIdleTimeoutIsAnError: an endpoint that opens the stream and then goes
// silent must be aborted by the idle watchdog, as an error. A total-duration
// http.Client timeout cannot express this — it either fires on healthy long turns
// or never fires on a stalled one.
func TestStreamIdleTimeoutIsAnError(t *testing.T) {
	srv := httptest.NewServer(sseHandler(func(w http.ResponseWriter, r *http.Request, flush func()) {
		fmt.Fprint(w, sseDelta("thinking"))
		flush()
		<-r.Context().Done() // then stall until the client gives up
	}))
	defer srv.Close()

	client := NewGatewayClient(srv.URL, "k")
	client.idleTimeout = 100 * time.Millisecond
	agent := NewSmoothAgent(client, AgentOptions{})

	stream, err := agent.RunStream(context.Background(), "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := drain(stream)
	if err == nil {
		t.Fatal("stalled stream reported as a clean turn: Err() == nil")
	}
	if !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("want an idle-timeout error, got %v", err)
	}
	for _, e := range events {
		if e.Kind == StreamDone {
			t.Fatal("stalled stream emitted StreamDone")
		}
	}
}

// TestClientTimeoutExpiryIsAnError pins the exact mechanism the bug report named:
// http.Client.Timeout covers reading the response BODY, so when it expires it tears
// the SSE body out mid-stream. That must surface as an error. (The value itself is
// now 600s, mirroring the reference; this drives it to 150ms to assert the
// BEHAVIOUR on expiry without a ten-minute test.)
func TestClientTimeoutExpiryIsAnError(t *testing.T) {
	srv := httptest.NewServer(sseHandler(func(w http.ResponseWriter, r *http.Request, flush func()) {
		for {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
			fmt.Fprint(w, sseDelta("x"))
			flush()
		}
	}))
	defer srv.Close()

	client := NewGatewayClient(srv.URL, "k")
	client.HTTPClient.Timeout = 150 * time.Millisecond
	agent := NewSmoothAgent(client, AgentOptions{})

	stream, err := agent.RunStream(context.Background(), "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := drain(stream)
	if err == nil {
		t.Fatal("client-timeout truncation reported as a clean turn: Err() == nil")
	}
	for _, e := range events {
		if e.Kind == StreamDone {
			t.Fatalf("client-timeout truncation emitted StreamDone with %q", e.Response.Text)
		}
	}
}

// TestStreamHealthyLongTurnIsNotTruncated is the other half of the timeout fix:
// a healthy turn must not be cut off just for taking a while. The 60s→600s ceiling
// change cannot be asserted without a ten-minute test, so this pins the property
// that makes the ceiling the wrong tool in the first place — the liveness deadline
// tracks the gap BETWEEN reads, not the total — by streaming a turn several times
// longer than its own idle window.
func TestStreamHealthyLongTurnIsNotTruncated(t *testing.T) {
	const pieces = 10
	srv := httptest.NewServer(sseHandler(func(w http.ResponseWriter, r *http.Request, flush func()) {
		for i := 0; i < pieces; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
			fmt.Fprint(w, sseDelta("x"))
			flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flush()
	}))
	defer srv.Close()

	client := NewGatewayClient(srv.URL, "k")
	// Total run is ~200ms — far past this ceiling had it been a total-duration
	// timeout — while no single gap comes close to it.
	client.idleTimeout = 100 * time.Millisecond
	agent := NewSmoothAgent(client, AgentOptions{})

	stream, err := agent.RunStream(context.Background(), "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := drain(stream)
	if err != nil {
		t.Fatalf("healthy long stream was truncated: %v", err)
	}
	done := events[len(events)-1]
	if done.Kind != StreamDone {
		t.Fatalf("want a terminal StreamDone, got %v", done.Kind)
	}
	if want := strings.Repeat("x", pieces); done.Response.Text != want {
		t.Fatalf("text truncated: got %q want %q", done.Response.Text, want)
	}
}

// TestOverLongSSELineIsAnError: a single `data:` line past the scanner's ceiling
// used to end the loop indistinguishably from a clean finish.
func TestOverLongSSELineIsAnError(t *testing.T) {
	srv := httptest.NewServer(sseHandler(func(w http.ResponseWriter, r *http.Request, flush func()) {
		fmt.Fprint(w, "data: ")
		// One unterminated line larger than the 8MB ceiling.
		blob := strings.Repeat("a", 64*1024)
		for i := 0; i < 160; i++ {
			fmt.Fprint(w, blob)
		}
		flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := NewGatewayClient(srv.URL, "k")
	agent := NewSmoothAgent(client, AgentOptions{})
	stream, err := agent.RunStream(context.Background(), "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drain(stream); err == nil {
		t.Fatal("over-long SSE line reported as a clean turn")
	}
}

// TestRunStreamRunsToolCallVeto: the SEP tool_call hook must gate the streaming
// path exactly as it gates Run. A veto honoured on one path and not the other is
// not a veto.
func TestRunStreamRunsToolCallVeto(t *testing.T) {
	tool := &recordingTool{name: "danger.wipe"}
	hooks := &fakeHooks{blockTool: "danger.wipe", blockReason: "not on my watch"}
	mock := NewMockLlmProvider()
	mock.PushResponse(ToolCallResponse("c1", "danger.wipe", `{"all":true}`)).
		PushResponse(TextResponse("ok"))
	agent := NewSmoothAgent(mock, AgentOptions{Tools: []Tool{tool}, Extensions: hooks})

	stream, err := agent.RunStream(context.Background(), "wipe it", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	if tool.calls != 0 {
		t.Fatalf("vetoed tool executed on the streaming path (%d calls)", tool.calls)
	}
	if len(hooks.hookedTools) == 0 {
		t.Fatal("tool_call hook was never invoked on the streaming path")
	}
	var sawBlockedResult bool
	for _, e := range events {
		if e.Kind == StreamToolResult && strings.Contains(e.Result, "not on my watch") {
			sawBlockedResult = true
		}
	}
	if !sawBlockedResult {
		t.Fatal("veto reason never surfaced as a tool result")
	}
}

// TestRunStreamRunsToolCallRewrite: the hook's argument rewrite must reach the
// tool AND the emitted tool_call event, so the UI shows what actually ran.
func TestRunStreamRunsToolCallRewrite(t *testing.T) {
	tool := &recordingTool{name: "search.web"}
	hooks := &fakeHooks{patchArgsFor: "search.web", patchArgs: `{"q":"redacted"}`}
	mock := NewMockLlmProvider()
	mock.PushResponse(ToolCallResponse("c1", "search.web", `{"q":"secret"}`)).
		PushResponse(TextResponse("ok"))
	agent := NewSmoothAgent(mock, AgentOptions{Tools: []Tool{tool}, Extensions: hooks})

	stream, err := agent.RunStream(context.Background(), "search", nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := tool.gotArgs["q"]; got != "redacted" {
		t.Fatalf("rewritten arguments never reached the tool: %v", got)
	}
	for _, e := range events {
		if e.Kind == StreamToolCall && !strings.Contains(e.Arguments, "redacted") {
			t.Fatalf("tool_call event shows pre-hook arguments: %q", e.Arguments)
		}
	}
}

// TestRunStreamDispatchesTurnEvents: extensions must see turn_start / message_end
// / turn_end for a streamed turn, or every extension's telemetry silently omits
// every streaming turn.
func TestRunStreamDispatchesTurnEvents(t *testing.T) {
	hooks := &fakeHooks{}
	mock := NewMockLlmProvider().PushResponse(TextResponse("hello"))
	agent := NewSmoothAgent(mock, AgentOptions{Extensions: hooks})

	stream, err := agent.RunStream(context.Background(), "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := drain(stream); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{sepTurnStart, sepMessageEnd, sepTurnEnd} {
		found := false
		for _, got := range hooks.events {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("streamed turn never dispatched %q (got %v)", want, hooks.events)
		}
	}
}

// TestAbandonedStreamReleasesGoroutineAndSocket: a consumer that stops ranging
// early and calls Close must free the turn goroutine and the model connection.
// Deliberately does NOT drain the channel — draining would let a wedged sender
// make progress and hide the very leak under test.
func TestAbandonedStreamReleasesGoroutineAndSocket(t *testing.T) {
	handlerReturned := make(chan struct{})
	srv := httptest.NewServer(sseHandler(func(w http.ResponseWriter, r *http.Request, flush func()) {
		defer close(handlerReturned)
		for i := 0; i < 10000; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(time.Millisecond):
			}
			fmt.Fprint(w, sseDelta("tick"))
			flush()
		}
	}))
	defer srv.Close()

	agent := NewSmoothAgent(NewGatewayClient(srv.URL, "k"), AgentOptions{})
	stream, err := agent.RunStream(context.Background(), "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-stream.Events() // take exactly one event, then walk away
	stream.Close()

	select {
	case <-handlerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("server handler still streaming after Close: the socket leaked")
	}

	noGoroutineMatching(t, "core.(*SmoothAgent).runStream")
	noGoroutineMatching(t, "core.(*GatewayClient).ChatStream")
}

// TestAbandonedStreamUnblocksOnContextCancel is the same leak via the other exit:
// cancelling the caller's own context, with no Close call.
func TestAbandonedStreamUnblocksOnContextCancel(t *testing.T) {
	mock := NewMockLlmProvider().PushResponse(TextResponse("a much longer answer that arrives in several chunks"))
	agent := NewSmoothAgent(mock, AgentOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := agent.RunStream(ctx, "hi", nil)
	if err != nil {
		t.Fatal(err)
	}
	<-stream.Events()
	cancel()

	noGoroutineMatching(t, "core.(*SmoothAgent).runStream")
	noGoroutineMatching(t, "core.(*MockLlmProvider).ChatStream")
}

// TestGatewayChatStreamGoroutineExitsOnAbandon exercises the ChatStream contract
// directly: the cost chunk was a BARE send, so a caller that never received it
// parked the producer before the scan loop even started. Cancelling ctx tears the
// SOCKET down either way (the request context reaches the transport), which is why
// this asserts on the goroutine — the part that actually leaked.
func TestGatewayChatStreamGoroutineExitsOnAbandon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-litellm-response-cost-margin-amount", "0.0042")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client := NewGatewayClient(srv.URL, "k")
	if _, err := client.ChatStream(ctx, ChatRequest{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	// Never receive from the channel — the cost chunk is waiting on it.
	cancel()

	noGoroutineMatching(t, "core.(*GatewayClient).ChatStream")
}
