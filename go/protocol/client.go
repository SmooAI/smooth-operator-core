// Package protocol is the Go client for the smooth-operator WebSocket protocol.
//
// It mirrors the TypeScript reference implementation: a transport-agnostic Client
// that correlates server events back to client actions by requestId, exposes the
// non-streaming actions (create session, get session, get messages, ping) as plain
// request/response calls, and models a send_message turn as a streaming MessageTurn
// — a channel of typed events plus an awaitable terminal eventual_response. HITL
// resumes (confirm_tool_action, verify_otp) route their resumed stream back into the
// original turn by requestId.
package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultTurnTimeout bounds a streaming send_message turn: if the server accepts the
// message but never emits a terminal eventual_response / error within this window,
// the turn settles with a *TurnTimeoutError instead of hanging forever. Override via
// Options.TurnTimeout (set it to a negative value to disable).
const DefaultTurnTimeout = 120 * time.Second

// Client is a transport-agnostic smooth-operator protocol client.
//
// It is safe for concurrent use. Construct it with New, call Connect, then issue
// actions. A single read loop dispatches inbound frames to the waiting request,
// active turn, or unsolicited-event listeners.
type Client struct {
	transport     Transport
	generateReqID func() string
	turnTimeout   time.Duration

	mu        sync.Mutex
	pending   map[string]*pendingRequest // single-response actions
	turns     map[string]*MessageTurn    // active streaming turns + HITL resumes
	listeners map[int]func(ServerEvent)  // unsolicited-event listeners
	nextLis   int
	closed    bool
	closeErr  error

	done chan struct{}
}

// Options configures a Client.
type Options struct {
	// Transport is the wire abstraction. Required (use NewWebSocketTransport for the
	// default, or a mock in tests).
	Transport Transport
	// GenerateRequestID overrides request ID generation. Defaults to "req-" + UUIDv4.
	GenerateRequestID func() string
	// TurnTimeout bounds a streaming send_message turn (see DefaultTurnTimeout).
	// Zero uses DefaultTurnTimeout; a negative value disables the turn timeout.
	TurnTimeout time.Duration
}

// New constructs a Client. It does not open the transport; call Connect.
func New(opts Options) (*Client, error) {
	if opts.Transport == nil {
		return nil, fmt.Errorf("smooth-agent: Options.Transport is required")
	}
	gen := opts.GenerateRequestID
	if gen == nil {
		gen = func() string { return "req-" + uuid.NewString() }
	}
	turnTimeout := opts.TurnTimeout
	switch {
	case turnTimeout == 0:
		turnTimeout = DefaultTurnTimeout
	case turnTimeout < 0:
		turnTimeout = 0 // disabled
	}
	return &Client{
		transport:     opts.Transport,
		generateReqID: gen,
		turnTimeout:   turnTimeout,
		pending:       make(map[string]*pendingRequest),
		turns:         make(map[string]*MessageTurn),
		listeners:     make(map[int]func(ServerEvent)),
		done:          make(chan struct{}),
	}, nil
}

// Connect opens the underlying transport and starts the dispatch loop.
func (c *Client) Connect(ctx context.Context) error {
	if err := c.transport.Connect(ctx); err != nil {
		return err
	}
	go c.dispatchLoop()
	return nil
}

// Close shuts the client down: it closes the transport and fails every in-flight
// request and turn.
func (c *Client) Close() error {
	err := c.transport.Close()
	c.failAll(fmt.Errorf("smooth-agent: client closed"))
	return err
}

// OnEvent registers a listener for unsolicited / uncorrelated server events (e.g.
// keepalive, server push). It returns an unsubscribe function.
func (c *Client) OnEvent(fn func(ServerEvent)) (unsubscribe func()) {
	c.mu.Lock()
	id := c.nextLis
	c.nextLis++
	c.listeners[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.listeners, id)
		c.mu.Unlock()
	}
}

// ───────────────────────────── pending request ──────────────────────────────

type pendingRequest struct {
	result chan ServerEvent
	err    chan error
}

// ─────────────────────────────── Actions ────────────────────────────────────

// CreateConversationSessionParams holds the caller-supplied fields for
// CreateConversationSession (action + requestId are filled in by the client).
type CreateConversationSessionParams struct {
	AgentID            string                 `json:"agentId"`
	UserName           string                 `json:"userName,omitempty"`
	UserEmail          string                 `json:"userEmail,omitempty"`
	BrowserFingerprint string                 `json:"browserFingerprint,omitempty"`
	Metadata           map[string]interface{} `json:"metadata,omitempty"`
	AuthContext        *RequestAuthContext    `json:"authContext,omitempty"`
}

// CreateConversationSession starts a new conversation session and returns the
// session descriptor carried in the immediate_response.
func (c *Client) CreateConversationSession(ctx context.Context, p CreateConversationSessionParams) (CreateConversationSessionResponse, error) {
	ev, err := c.request(ctx, ActionCreateConversationSession, p)
	if err != nil {
		return CreateConversationSessionResponse{}, err
	}
	return extractImmediateData[CreateConversationSessionResponse](ev)
}

// GetSessionParams holds the caller-supplied fields for GetSession.
type GetSessionParams struct {
	SessionID string `json:"sessionId"`
}

// GetSession fetches a session snapshot by ID.
func (c *Client) GetSession(ctx context.Context, p GetSessionParams) (GetSessionResponse, error) {
	ev, err := c.request(ctx, ActionGetSession, p)
	if err != nil {
		return GetSessionResponse{}, err
	}
	return extractImmediateData[GetSessionResponse](ev)
}

// GetMessagesParams holds the caller-supplied fields for GetMessages.
type GetMessagesParams struct {
	SessionID string `json:"sessionId"`
	Limit     int    `json:"limit,omitempty"`
	Before    string `json:"before,omitempty"`
}

// GetMessages fetches a page of conversation messages.
func (c *Client) GetMessages(ctx context.Context, p GetMessagesParams) (GetMessagesResponse, error) {
	ev, err := c.request(ctx, ActionGetConversationMessages, p)
	if err != nil {
		return GetMessagesResponse{}, err
	}
	return extractImmediateData[GetMessagesResponse](ev)
}

// Ping issues a keepalive ping and returns the server timestamp from the pong.
func (c *Client) Ping(ctx context.Context) (int, error) {
	ev, err := c.request(ctx, ActionPing, struct{}{})
	if err != nil {
		return 0, err
	}
	if ev.Type == EventPong {
		pong, derr := ev.AsPong()
		if derr == nil {
			if pong.Timestamp != nil {
				return *pong.Timestamp, nil
			}
			if pong.Data != nil {
				return pong.Data.Timestamp, nil
			}
		}
	}
	return 0, nil
}

// SendMessageParams holds the caller-supplied fields for SendMessage.
type SendMessageParams struct {
	SessionID string `json:"sessionId"`
	Message   string `json:"message"`
	// Stream controls whether incremental stream_chunk/stream_token events are sent.
	// nil leaves the field off the wire (server default = true).
	Stream *bool `json:"stream,omitempty"`
}

// SendMessage submits a user message and returns a MessageTurn. Range over
// turn.Events() to receive each streamed event in order, or call turn.Wait(ctx) to
// block for the terminal eventual_response.
func (c *Client) SendMessage(p SendMessageParams) *MessageTurn {
	requestID := c.generateReqID()
	turn := newMessageTurn(requestID, c.turnTimeout, func() { c.removeTurn(requestID) })

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		turn.abort(c.closeErr)
		return turn
	}
	c.turns[requestID] = turn
	c.mu.Unlock()

	frame := mergeAction(ActionSendMessage, requestID, p)
	if err := c.transport.Send(frame); err != nil {
		c.removeTurn(requestID)
		turn.abort(err)
	}
	return turn
}

// ConfirmToolActionParams holds the caller-supplied fields for ConfirmToolAction.
type ConfirmToolActionParams struct {
	SessionID string `json:"sessionId"`
	// RequestID must match the write_confirmation_required event being answered.
	RequestID string `json:"requestId"`
	Approved  bool   `json:"approved"`
}

// ConfirmToolAction approves or rejects a pending tool write, resuming the paused
// turn identified by p.RequestID. The resumed stream flows back into that turn.
func (c *Client) ConfirmToolAction(p ConfirmToolActionParams) error {
	frame, err := json.Marshal(struct {
		Action ActionType `json:"action"`
		ConfirmToolActionParams
	}{Action: ActionConfirmToolAction, ConfirmToolActionParams: p})
	if err != nil {
		return err
	}
	return c.transport.Send(frame)
}

// VerifyOTPParams holds the caller-supplied fields for VerifyOTP.
type VerifyOTPParams struct {
	SessionID string `json:"sessionId"`
	// RequestID must match the otp_verification_required event being answered.
	RequestID string `json:"requestId"`
	Code      string `json:"code"`
}

// VerifyOTP submits an OTP code, resuming the paused turn identified by
// p.RequestID. The resumed stream flows back into that turn.
func (c *Client) VerifyOTP(p VerifyOTPParams) error {
	frame, err := json.Marshal(struct {
		Action ActionType `json:"action"`
		VerifyOTPParams
	}{Action: ActionVerifyOTP, VerifyOTPParams: p})
	if err != nil {
		return err
	}
	return c.transport.Send(frame)
}

// ─────────────────────────────── Internals ──────────────────────────────────

// request sends an action expecting a single correlated response and blocks until
// that response arrives, ctx is cancelled, or the transport closes.
func (c *Client) request(ctx context.Context, action ActionType, params any) (ServerEvent, error) {
	requestID := c.generateReqID()

	pr := &pendingRequest{
		result: make(chan ServerEvent, 1),
		err:    make(chan error, 1),
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ServerEvent{}, c.closeErr
	}
	c.pending[requestID] = pr
	c.mu.Unlock()

	frame := mergeAction(action, requestID, params)
	if err := c.transport.Send(frame); err != nil {
		c.removePending(requestID)
		return ServerEvent{}, err
	}

	select {
	case ev := <-pr.result:
		return ev, nil
	case err := <-pr.err:
		return ServerEvent{}, err
	case <-ctx.Done():
		c.removePending(requestID)
		return ServerEvent{}, ctx.Err()
	}
}

// dispatchLoop reads frames off the transport and routes them.
func (c *Client) dispatchLoop() {
	recv := c.transport.Receive()
	for frame := range recv {
		ev, err := ParseServerEvent(frame)
		if err != nil {
			continue // ignore malformed / unknown frames
		}
		c.route(ev)
	}
	// Transport closed: fail everything still in flight.
	err := c.transport.Err()
	if err == nil {
		err = fmt.Errorf("smooth-agent: transport closed")
	}
	c.failAll(err)
}

// route delivers an event to its turn, pending request, or listeners.
func (c *Client) route(ev ServerEvent) {
	reqID := ev.RequestID

	c.mu.Lock()
	if reqID != "" {
		if turn, ok := c.turns[reqID]; ok {
			c.mu.Unlock()
			turn.push(ev)
			return
		}
		if pr, ok := c.pending[reqID]; ok {
			delete(c.pending, reqID)
			c.mu.Unlock()
			if ev.Type == EventError {
				pr.err <- protocolErrorFromEvent(ev)
			} else {
				pr.result <- ev
			}
			return
		}
	}
	// Unsolicited / uncorrelated event — snapshot listeners under lock.
	ls := make([]func(ServerEvent), 0, len(c.listeners))
	for _, fn := range c.listeners {
		ls = append(ls, fn)
	}
	c.mu.Unlock()
	for _, fn := range ls {
		fn(ev)
	}
}

func (c *Client) removePending(requestID string) {
	c.mu.Lock()
	delete(c.pending, requestID)
	c.mu.Unlock()
}

func (c *Client) removeTurn(requestID string) {
	c.mu.Lock()
	delete(c.turns, requestID)
	c.mu.Unlock()
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = err
	pending := c.pending
	turns := c.turns
	c.pending = make(map[string]*pendingRequest)
	c.turns = make(map[string]*MessageTurn)
	close(c.done)
	c.mu.Unlock()

	for _, pr := range pending {
		pr.err <- err
	}
	for _, turn := range turns {
		turn.abort(err)
	}
}

// mergeAction marshals params and injects the action + requestId envelope fields.
// params is marshalled to an object and the two envelope keys are merged in.
func mergeAction(action ActionType, requestID string, params any) []byte {
	// Marshal params into a generic map so we can splice in the envelope fields
	// regardless of the concrete params struct.
	raw, err := json.Marshal(params)
	if err != nil {
		// A params struct that fails to marshal is a programmer error; emit a minimal
		// frame so the server replies with a validation error rather than panicking.
		raw = []byte("{}")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		m = map[string]json.RawMessage{}
	}
	m["action"], _ = json.Marshal(string(action))
	m["requestId"], _ = json.Marshal(requestID)
	out, _ := json.Marshal(m)
	return out
}

// extractImmediateData pulls the typed data payload out of an immediate_response.
func extractImmediateData[T any](ev ServerEvent) (T, error) {
	var zero T
	if ev.Type == EventError {
		return zero, protocolErrorFromEvent(ev)
	}
	// The payload lives in `data` for immediate_response (and any non-streaming ack).
	var wrap struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(ev.Raw, &wrap); err != nil {
		return zero, err
	}
	if len(wrap.Data) == 0 || string(wrap.Data) == "null" {
		return zero, fmt.Errorf("smooth-agent: %s event carried no data payload", ev.Type)
	}
	var v T
	if err := json.Unmarshal(wrap.Data, &v); err != nil {
		return zero, err
	}
	return v, nil
}
