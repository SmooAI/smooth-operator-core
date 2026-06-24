package protocol

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ProtocolError surfaces a server `error` event as a Go error.
type ProtocolError struct {
	Code      string
	Message   string
	RequestID string
}

func (e *ProtocolError) Error() string {
	if e.RequestID != "" {
		return fmt.Sprintf("smooth-agent: protocol error %s: %s (requestId=%s)", e.Code, e.Message, e.RequestID)
	}
	return fmt.Sprintf("smooth-agent: protocol error %s: %s", e.Code, e.Message)
}

// TurnTimeoutError is the error a turn settles with when no terminal
// eventual_response / error arrives within the configured turn timeout. The turn's
// Events() channel is closed and Wait/result return this error.
type TurnTimeoutError struct {
	RequestID string
	Timeout   time.Duration
}

func (e *TurnTimeoutError) Error() string {
	return fmt.Sprintf("smooth-agent: turn %s timed out after %s without a terminal response", e.RequestID, e.Timeout)
}

// protocolErrorFromEvent builds a ProtocolError from an error event, tolerating
// either the nested data.error or the envelope-level error shape.
func protocolErrorFromEvent(ev ServerEvent) *ProtocolError {
	pe := &ProtocolError{Code: "INTERNAL_ERROR", Message: "Unknown protocol error", RequestID: ev.RequestID}
	if errEv, err := ev.AsError(); err == nil {
		if errEv.Data.Error.Code != "" {
			pe.Code = errEv.Data.Error.Code
			pe.Message = errEv.Data.Error.Message
		} else if errEv.Error != nil && errEv.Error.Code != "" {
			pe.Code = errEv.Error.Code
			pe.Message = errEv.Error.Message
		}
	}
	return pe
}

// MessageTurn is a single streaming send_message turn. Receive each intermediate
// event in arrival order from Events(), or block for the terminal eventual_response
// with Wait(ctx). HITL resumes (confirm_tool_action / verify_otp) for the same
// requestId flow back into the same turn.
//
//	turn := client.SendMessage(protocol.SendMessageParams{SessionID: id, Message: "hi"})
//	for ev := range turn.Events() {
//	    if ev.Type == protocol.EventStreamToken {
//	        tok, _ := ev.AsStreamToken()
//	        fmt.Print(tok.Token)
//	    }
//	}
//	final, err := turn.Wait(context.Background())
type MessageTurn struct {
	requestID string
	onClose   func()

	events chan ServerEvent

	// sendMu serializes sends-to and the close-of t.events so a concurrent finish()
	// (e.g. via Client.Close → failAll → abort) can never close the channel while
	// push() is mid-send. push() bails out under this lock once closed is set, so a
	// settled turn neither blocks nor panics on a late event.
	sendMu sync.Mutex
	closed bool // events channel has been closed (guarded by sendMu)

	mu        sync.Mutex
	done      bool
	final     *EventualResponse
	failErr   error
	settled   chan struct{} // closed once the turn finishes
	closeOnce sync.Once

	// timeout fires the turn idle/overall timeout. nil when no timeout is configured.
	timer   *time.Timer
	timeout time.Duration
}

func newMessageTurn(requestID string, timeout time.Duration, onClose func()) *MessageTurn {
	t := &MessageTurn{
		requestID: requestID,
		onClose:   onClose,
		events:    make(chan ServerEvent, 64),
		settled:   make(chan struct{}),
		timeout:   timeout,
	}
	if timeout > 0 {
		t.timer = time.AfterFunc(timeout, t.onTimeout)
	}
	return t
}

// RequestID is the correlation ID this turn is keyed on.
func (t *MessageTurn) RequestID() string { return t.requestID }

// Events returns the channel of streamed events. It is closed when the turn ends
// (after the terminal event has been delivered, or on abort / timeout).
func (t *MessageTurn) Events() <-chan ServerEvent { return t.events }

// Wait blocks until the turn produces its terminal eventual_response, the turn
// fails (error event / transport close / timeout), or ctx is cancelled.
func (t *MessageTurn) Wait(ctx context.Context) (EventualResponse, error) {
	select {
	case <-t.settled:
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.failErr != nil {
			return EventualResponse{}, t.failErr
		}
		if t.final != nil {
			return *t.final, nil
		}
		return EventualResponse{}, fmt.Errorf("smooth-agent: turn ended without a terminal response")
	case <-ctx.Done():
		return EventualResponse{}, ctx.Err()
	}
}

// onTimeout settles the turn with a TurnTimeoutError when no terminal event has
// arrived in time.
func (t *MessageTurn) onTimeout() {
	t.finish(nil, &TurnTimeoutError{RequestID: t.requestID, Timeout: t.timeout})
}

// push feeds an event into the turn. Called by the client dispatcher.
//
// It delivers the event under sendMu and selects on t.settled so that a turn which
// has already settled (terminal event, abort, or timeout) — and therefore had its
// events channel closed — never blocks a stuck caller's dispatch goroutine and never
// sends on a closed channel.
func (t *MessageTurn) push(ev ServerEvent) {
	t.mu.Lock()
	done := t.done
	t.mu.Unlock()
	if done {
		return
	}

	t.sendMu.Lock()
	if t.closed {
		t.sendMu.Unlock()
		return
	}
	// select on settled so a turn that settles concurrently (and is about to close
	// the channel under sendMu, which we hold) cannot wedge this send. The buffered
	// channel keeps the common path non-blocking; a slow/un-ranged consumer that
	// fills the buffer parks here until it drains or the turn settles — without ever
	// blocking other turns, because each turn is pushed from route() inline only for
	// its own requestId and Client.Close settles it via abort().
	select {
	case t.events <- ev:
	case <-t.settled:
		t.sendMu.Unlock()
		return
	}
	t.sendMu.Unlock()

	switch ev.Type {
	case EventError:
		t.finish(nil, protocolErrorFromEvent(ev))
	case EventEventualResponse:
		final, err := ev.AsEventualResponse()
		if err != nil {
			t.finish(nil, err)
			return
		}
		t.finish(&final, nil)
	}
}

// abort force-closes the turn with an error (e.g. on disconnect).
func (t *MessageTurn) abort(err error) {
	t.finish(nil, err)
}

// finish settles the turn exactly once, recording the outcome and closing channels.
func (t *MessageTurn) finish(final *EventualResponse, err error) {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return
	}
	t.done = true
	t.final = final
	t.failErr = err
	t.mu.Unlock()

	if t.timer != nil {
		t.timer.Stop()
	}
	if t.onClose != nil {
		t.onClose()
	}
	t.closeOnce.Do(func() {
		// Signal settled first so any push() parked on the select unblocks via the
		// <-t.settled branch instead of racing the close below.
		close(t.settled)
		// Take sendMu so the close of t.events is mutually exclusive with any
		// in-flight push() send; once closed is set, later push() calls bail out.
		t.sendMu.Lock()
		t.closed = true
		close(t.events)
		t.sendMu.Unlock()
	})
}
