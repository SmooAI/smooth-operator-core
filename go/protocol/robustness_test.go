package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// makeClientWithTimeout builds a client whose streaming turns use the given turn
// timeout. A negative timeout disables it; zero falls back to DefaultTurnTimeout.
func makeClientWithTimeout(t *testing.T, turnTimeout time.Duration) (*Client, *mockTransport) {
	t.Helper()
	tr := newMockTransport()
	var n int
	c, err := New(Options{
		Transport:         tr,
		GenerateRequestID: func() string { n++; return "req-test-" + itoa(n) },
		TurnTimeout:       turnTimeout,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return c, tr
}

// TestConcurrentFinishDuringPush reproduces Bug 1: a turn finishing (via
// Client.Close → failAll → abort → finish, which close()s the events channel) on one
// goroutine while the dispatcher is mid-push() on another must not panic with "send
// on closed channel" and must not deadlock. Run under -race.
//
// push() and finish() are the package-internal unit under test, so we drive them
// directly — a faithful model of route()→turn.push (dispatch goroutine) racing
// failAll→abort→finish (Close goroutine), without the mock transport's channel-close
// artifact. A nobody-is-ranging consumer (the common Wait()-only caller) means the
// buffered events channel fills and push() parks in its select — exactly the state in
// which a naive close(t.events) would panic the pusher.
func TestConcurrentFinishDuringPush(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		turn := newMessageTurn("req-race", 0 /* no timeout */, func() {})

		ev := mustParse(t, map[string]any{
			"type": "stream_token", "requestId": "req-race", "token": "x",
			"data": map[string]any{"requestId": "req-race", "token": "x"},
		})

		var wg sync.WaitGroup
		// Several pushers flood the turn; the 64-deep buffer fills (nobody ranges),
		// so pushes park in the settled-select — the exact close-vs-send window.
		const pushers = 4
		for p := 0; p < pushers; p++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 100; j++ {
					turn.push(ev)
				}
			}()
		}
		// Concurrently settle the turn (closes t.events) from another goroutine.
		wg.Add(1)
		go func() {
			defer wg.Done()
			turn.abort(errors.New("client closed"))
		}()

		// Bound the whole iteration so a deadlock is caught (not just a panic).
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iter %d: push/finish deadlocked", iter)
		}

		// The turn must be settled with the abort error.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := turn.Wait(ctx)
		cancel()
		if err == nil {
			t.Fatalf("iter %d: turn settled without error", iter)
		}
	}
}

// mustParse parses an event map into a ServerEvent via the real frame parser.
func mustParse(t *testing.T, v map[string]any) ServerEvent {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ev, err := ParseServerEvent(b)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return ev
}

// TestStreamingTurnTimesOut reproduces Bug 2 (Go): the server accepts send_message
// but never emits a terminal eventual_response / error. With a turn timeout the turn
// must settle with a *TurnTimeoutError within the bound, Wait() must return it, and
// Events() must close (so a `for range` terminates).
func TestStreamingTurnTimesOut(t *testing.T) {
	c, tr := makeClientWithTimeout(t, 80*time.Millisecond)
	defer c.Close()

	turn := c.SendMessage(SendMessageParams{SessionID: "s", Message: "hang"})
	reqID := turn.RequestID()

	// One intermediate event arrives, but no terminal event ever does.
	tr.emit(t, map[string]any{
		"type": "stream_token", "requestId": reqID, "token": "partial",
		"data": map[string]any{"requestId": reqID, "token": "partial"},
	})

	// Events() must close once the turn times out (otherwise a range hangs forever).
	eventsClosed := make(chan struct{})
	go func() {
		for range turn.Events() {
		}
		close(eventsClosed)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := turn.Wait(ctx)
	var te *TurnTimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected *TurnTimeoutError, got %v", err)
	}
	if te.RequestID != reqID {
		t.Errorf("timeout requestId = %q, want %q", te.RequestID, reqID)
	}

	select {
	case <-eventsClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("Events() channel did not close after turn timeout")
	}
}

// TestStreamingTurnTimeoutClearedOnTerminal asserts the timeout does not fire when a
// terminal event arrives first (regression guard against a stray late settle).
func TestStreamingTurnTimeoutClearedOnTerminal(t *testing.T) {
	c, tr := makeClientWithTimeout(t, 100*time.Millisecond)
	defer c.Close()

	turn := c.SendMessage(SendMessageParams{SessionID: "s", Message: "ok"})
	reqID := turn.RequestID()

	tr.emit(t, map[string]any{
		"type": "eventual_response", "requestId": reqID, "status": 200,
		"data": map[string]any{"requestId": reqID, "status": 200, "data": map[string]any{"messageId": "m", "response": nil}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatalf("turn settled with unexpected error: %v", err)
	}

	// Give the (now-stopped) timer's deadline a chance to pass; the turn must remain
	// settled with the eventual_response, not flip to a timeout error.
	time.Sleep(150 * time.Millisecond)
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatalf("turn flipped to error after terminal response: %v", err)
	}
}
