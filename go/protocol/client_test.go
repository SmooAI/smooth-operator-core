package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// mockTransport is an in-memory Transport: it records sent frames and lets the test
// inject server events. No real socket is involved.
type mockTransport struct {
	mu     sync.Mutex
	sent   [][]byte
	recv   chan []byte
	open   bool
	closed bool
	err    error
}

func newMockTransport() *mockTransport {
	return &mockTransport{recv: make(chan []byte, 64)}
}

func (m *mockTransport) Connect(ctx context.Context) error {
	m.mu.Lock()
	m.open = true
	m.mu.Unlock()
	return nil
}

func (m *mockTransport) Send(frame []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.open || m.closed {
		return ErrTransportClosed
	}
	cp := make([]byte, len(frame))
	copy(cp, frame)
	m.sent = append(m.sent, cp)
	return nil
}

func (m *mockTransport) Receive() <-chan []byte { return m.recv }

func (m *mockTransport) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	m.closed = true
	close(m.recv)
	return nil
}

func (m *mockTransport) Err() error { return m.err }

// emit injects a server→client event frame.
func (m *mockTransport) emit(t *testing.T, ev map[string]any) {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal emit: %v", err)
	}
	m.recv <- b
}

// lastSent parses the most recently sent action frame.
func (m *mockTransport) lastSent(t *testing.T) map[string]any {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		t.Fatal("no frame sent")
	}
	var out map[string]any
	if err := json.Unmarshal(m.sent[len(m.sent)-1], &out); err != nil {
		t.Fatalf("parse last sent: %v", err)
	}
	return out
}

func makeClient(t *testing.T) (*Client, *mockTransport) {
	t.Helper()
	tr := newMockTransport()
	var n int
	c, err := New(Options{
		Transport:         tr,
		GenerateRequestID: func() string { n++; return "req-test-" + itoa(n) },
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return c, tr
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestSendMessageStreamingOrder drives a full send_message turn and asserts the
// typed events arrive in order on the channel and the terminal resolves.
func TestSendMessageStreamingOrder(t *testing.T) {
	c, tr := makeClient(t)
	defer c.Close()

	turn := c.SendMessage(SendMessageParams{SessionID: "sess-1", Message: "hi", Stream: ptr(true)})
	reqID := turn.RequestID()

	sent := tr.lastSent(t)
	if sent["action"] != string(ActionSendMessage) || sent["sessionId"] != "sess-1" || sent["message"] != "hi" {
		t.Fatalf("unexpected send_message frame: %v", sent)
	}
	if sent["requestId"] != reqID {
		t.Fatalf("frame requestId %v != turn requestId %v", sent["requestId"], reqID)
	}

	// Collect streamed events in a background goroutine.
	var collected []EventType
	var tokens string
	done := make(chan struct{})
	go func() {
		for ev := range turn.Events() {
			collected = append(collected, ev.Type)
			if ev.Type == EventStreamToken {
				tok, err := ev.AsStreamToken()
				if err == nil {
					tokens += tok.Data.Token
				}
			}
		}
		close(done)
	}()

	tr.emit(t, map[string]any{"type": "stream_token", "requestId": reqID, "token": "Hel", "data": map[string]any{"requestId": reqID, "token": "Hel"}})
	tr.emit(t, map[string]any{"type": "stream_token", "requestId": reqID, "token": "lo", "data": map[string]any{"requestId": reqID, "token": "lo"}})
	tr.emit(t, map[string]any{
		"type": "stream_chunk", "requestId": reqID, "node": "response_composer",
		"data": map[string]any{"requestId": reqID, "node": "response_composer", "state": map[string]any{"structuredResponse": map[string]any{"responseParts": []string{"Hello"}}}},
	})
	tr.emit(t, map[string]any{
		"type": "eventual_response", "requestId": reqID, "status": 200,
		"data": map[string]any{"requestId": reqID, "status": 200, "data": map[string]any{"messageId": "msg-1", "response": map[string]any{"responseParts": []string{"Hello"}}, "needsEscalation": false}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	final, err := turn.Wait(ctx)
	if err != nil {
		t.Fatalf("turn.Wait: %v", err)
	}
	<-done

	if final.Type != "eventual_response" {
		t.Errorf("final.Type = %q", final.Type)
	}
	if final.Data.Data.MessageID != "msg-1" {
		t.Errorf("final messageId = %q", final.Data.Data.MessageID)
	}

	want := []EventType{EventStreamToken, EventStreamToken, EventStreamChunk, EventEventualResponse}
	if len(collected) != len(want) {
		t.Fatalf("collected %v, want %v", collected, want)
	}
	for i := range want {
		if collected[i] != want[i] {
			t.Errorf("event[%d] = %q, want %q", i, collected[i], want[i])
		}
	}
	if tokens != "Hello" {
		t.Errorf("accumulated tokens = %q, want %q", tokens, "Hello")
	}
}

// TestSendMessageBuffersBeforeIteration asserts events emitted before anyone reads
// the channel are buffered (not lost).
func TestSendMessageBuffersBeforeIteration(t *testing.T) {
	c, tr := makeClient(t)
	defer c.Close()

	turn := c.SendMessage(SendMessageParams{SessionID: "s", Message: "q"})
	reqID := turn.RequestID()

	tr.emit(t, map[string]any{"type": "stream_token", "requestId": reqID, "token": "A", "data": map[string]any{"requestId": reqID, "token": "A"}})
	tr.emit(t, map[string]any{"type": "eventual_response", "requestId": reqID, "status": 200,
		"data": map[string]any{"requestId": reqID, "status": 200, "data": map[string]any{"messageId": "m", "response": nil}}})

	// Wait for terminal first, then drain the buffered channel.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	var types []EventType
	for ev := range turn.Events() {
		types = append(types, ev.Type)
	}
	if len(types) != 2 || types[0] != EventStreamToken || types[1] != EventEventualResponse {
		t.Fatalf("types = %v", types)
	}
}

// TestSendMessageErrorEvent asserts an error event rejects the turn with a ProtocolError.
func TestSendMessageErrorEvent(t *testing.T) {
	c, tr := makeClient(t)
	defer c.Close()

	turn := c.SendMessage(SendMessageParams{SessionID: "s", Message: "boom"})
	reqID := turn.RequestID()

	tr.emit(t, map[string]any{"type": "error", "requestId": reqID,
		"data": map[string]any{"requestId": reqID, "error": map[string]any{"code": "RATE_LIMITED", "message": "slow down"}}})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := turn.Wait(ctx)
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProtocolError, got %v", err)
	}
	if pe.Code != "RATE_LIMITED" {
		t.Errorf("code = %q", pe.Code)
	}
}

// TestHITLConfirmResume asserts a write_confirmation_required pauses the turn and a
// confirm_tool_action resume completes the same turn.
func TestHITLConfirmResume(t *testing.T) {
	c, tr := makeClient(t)
	defer c.Close()

	turn := c.SendMessage(SendMessageParams{SessionID: "s", Message: "delete it"})
	reqID := turn.RequestID()

	var seen []EventType
	done := make(chan struct{})
	go func() {
		for ev := range turn.Events() {
			seen = append(seen, ev.Type)
		}
		close(done)
	}()

	tr.emit(t, map[string]any{"type": "write_confirmation_required", "requestId": reqID,
		"data": map[string]any{"requestId": reqID, "data": map[string]any{"toolId": "t1", "actionDescription": "Delete contact"}}})

	// Give the dispatcher a moment to deliver the HITL event before confirming.
	time.Sleep(20 * time.Millisecond)

	if err := c.ConfirmToolAction(ConfirmToolActionParams{SessionID: "s", RequestID: reqID, Approved: true}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	sent := tr.lastSent(t)
	if sent["action"] != string(ActionConfirmToolAction) || sent["approved"] != true || sent["requestId"] != reqID {
		t.Fatalf("unexpected confirm frame: %v", sent)
	}

	tr.emit(t, map[string]any{"type": "eventual_response", "requestId": reqID, "status": 200,
		"data": map[string]any{"requestId": reqID, "status": 200, "data": map[string]any{"messageId": "m", "response": nil}}})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	<-done

	want := []EventType{EventWriteConfirmationRequired, EventEventualResponse}
	if len(seen) != len(want) || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("seen = %v, want %v", seen, want)
	}
}

// TestCreateConversationSession asserts the immediate_response data decodes into the response type.
func TestCreateConversationSession(t *testing.T) {
	c, tr := makeClient(t)
	defer c.Close()

	type result struct {
		resp CreateConversationSessionResponse
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := c.CreateConversationSession(context.Background(), CreateConversationSessionParams{AgentID: "agent-1", UserName: "Alice"})
		resCh <- result{resp, err}
	}()

	// Wait for the request frame, grab its requestId.
	reqID := waitForLastRequestID(t, tr)
	sent := tr.lastSent(t)
	if sent["action"] != string(ActionCreateConversationSession) || sent["agentId"] != "agent-1" {
		t.Fatalf("unexpected create frame: %v", sent)
	}

	tr.emit(t, map[string]any{"type": "immediate_response", "requestId": reqID, "status": 200,
		"data": map[string]any{"sessionId": "sess-9", "conversationId": "conv-9", "agentId": "agent-1", "agentName": "Aria", "userParticipantId": "u-9", "agentParticipantId": "a-9"}})

	r := <-resCh
	if r.err != nil {
		t.Fatalf("create: %v", r.err)
	}
	if r.resp.SessionID != "sess-9" || r.resp.AgentName != "Aria" {
		t.Errorf("resp = %+v", r.resp)
	}
}

// TestPing asserts ping resolves with the pong timestamp.
func TestPing(t *testing.T) {
	c, tr := makeClient(t)
	defer c.Close()

	resCh := make(chan int, 1)
	go func() {
		ts, _ := c.Ping(context.Background())
		resCh <- ts
	}()

	reqID := waitForLastRequestID(t, tr)
	tr.emit(t, map[string]any{"type": "pong", "requestId": reqID, "timestamp": 1700000000000})
	if ts := <-resCh; ts != 1700000000000 {
		t.Errorf("ping ts = %d", ts)
	}
}

// TestNoCrossCorrelation asserts two concurrent requests resolve independently even
// when answered out of order.
func TestNoCrossCorrelation(t *testing.T) {
	c, tr := makeClient(t)
	defer c.Close()

	sessionData := func(id string) map[string]any {
		return map[string]any{"sessionId": id, "conversationId": "c", "agentId": "a", "agentName": "N", "userParticipantId": "u", "agentParticipantId": "ag"}
	}

	r1 := make(chan GetSessionResponse, 1)
	r2 := make(chan GetSessionResponse, 1)
	go func() { resp, _ := c.GetSession(context.Background(), GetSessionParams{SessionID: "s1"}); r1 <- resp }()
	req1 := waitForLastRequestID(t, tr)
	go func() { resp, _ := c.GetSession(context.Background(), GetSessionParams{SessionID: "s2"}); r2 <- resp }()
	req2 := waitForDifferentRequestID(t, tr, req1)

	if req1 == req2 {
		t.Fatal("request IDs collided")
	}

	// Answer out of order.
	tr.emit(t, map[string]any{"type": "immediate_response", "requestId": req2, "status": 200, "data": sessionData("s2")})
	tr.emit(t, map[string]any{"type": "immediate_response", "requestId": req1, "status": 200, "data": sessionData("s1")})

	if resp := <-r1; resp.SessionID != "s1" {
		t.Errorf("r1 sessionId = %q", resp.SessionID)
	}
	if resp := <-r2; resp.SessionID != "s2" {
		t.Errorf("r2 sessionId = %q", resp.SessionID)
	}
}

// TestKeepaliveListener asserts uncorrelated events go to OnEvent listeners.
func TestKeepaliveListener(t *testing.T) {
	c, tr := makeClient(t)
	defer c.Close()

	got := make(chan ServerEvent, 1)
	c.OnEvent(func(ev ServerEvent) { got <- ev })

	tr.emit(t, map[string]any{"type": "keepalive", "data": map[string]any{"requestId": "whatever"}})

	select {
	case ev := <-got:
		if ev.Type != EventKeepalive {
			t.Errorf("listener got %q", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("keepalive listener not invoked")
	}
}

// TestTransportCloseFailsPending asserts pending requests fail when the transport closes.
func TestTransportCloseFailsPending(t *testing.T) {
	c, tr := makeClient(t)

	errCh := make(chan error, 1)
	go func() { _, err := c.GetSession(context.Background(), GetSessionParams{SessionID: "s"}); errCh <- err }()
	waitForLastRequestID(t, tr)

	_ = tr.Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected error after transport close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending request did not fail on close")
	}
}

func ptr[T any](v T) *T { return &v }

// waitForLastRequestID polls until a frame has been sent and returns its requestId.
func waitForLastRequestID(t *testing.T, tr *mockTransport) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tr.mu.Lock()
		n := len(tr.sent)
		var frame []byte
		if n > 0 {
			frame = tr.sent[n-1]
		}
		tr.mu.Unlock()
		if frame != nil {
			var m map[string]any
			if json.Unmarshal(frame, &m) == nil {
				if id, ok := m["requestId"].(string); ok {
					return id
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for sent frame")
	return ""
}

// waitForDifferentRequestID polls until a frame with a requestId != prev is sent.
func waitForDifferentRequestID(t *testing.T, tr *mockTransport, prev string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tr.mu.Lock()
		n := len(tr.sent)
		var frame []byte
		if n > 0 {
			frame = tr.sent[n-1]
		}
		tr.mu.Unlock()
		if frame != nil {
			var m map[string]any
			if json.Unmarshal(frame, &m) == nil {
				if id, ok := m["requestId"].(string); ok && id != prev {
					return id
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for second sent frame")
	return ""
}
