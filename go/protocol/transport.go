package protocol

import (
	"context"
	"errors"
	"sync"

	"github.com/coder/websocket"
)

// Transport is the injectable wire abstraction the Client talks to. It is
// deliberately decoupled from any concrete WebSocket implementation so the client
// can be unit-tested with an in-memory mock and run against a real socket in
// production.
//
// A Transport must be safe for concurrent use: Send may be called from one
// goroutine while the receive loop delivers frames on another.
type Transport interface {
	// Connect opens the underlying connection. It must return once the transport is
	// ready to Send/Receive, or with an error if it cannot connect.
	Connect(ctx context.Context) error
	// Send writes a single serialized frame. It returns an error if the transport is
	// not open.
	Send(frame []byte) error
	// Receive returns the channel on which inbound frames are delivered. The channel
	// is closed when the transport closes (cleanly or on error).
	Receive() <-chan []byte
	// Close shuts the transport down. Subsequent Sends return an error and Receive's
	// channel is closed.
	Close() error
	// Err returns the terminal error that closed the transport, if any. It returns
	// nil for a clean close.
	Err() error
}

// ErrTransportClosed is returned by Send when the transport is no longer open.
var ErrTransportClosed = errors.New("smooth-agent: transport closed")

// WebSocketTransport is the default Transport, backed by github.com/coder/websocket
// (a small, dependency-light, context-aware WebSocket library). It dials lazily in
// Connect and pumps inbound text frames onto Receive's channel.
type WebSocketTransport struct {
	url  string
	opts *websocket.DialOptions

	mu     sync.Mutex
	conn   *websocket.Conn
	recv   chan []byte
	closed bool
	err    error

	// ctx governs the lifetime of the read loop and outbound writes.
	ctx    context.Context
	cancel context.CancelFunc
}

// NewWebSocketTransport builds a default WebSocket transport for the given URL
// (e.g. "wss://realtime.prod.smooth-agent.dev"). Pass opts to customise the dial
// (headers, subprotocols, …); nil is fine for defaults.
func NewWebSocketTransport(url string, opts *websocket.DialOptions) *WebSocketTransport {
	return &WebSocketTransport{
		url:  url,
		opts: opts,
		recv: make(chan []byte, 64),
	}
}

// Connect dials the WebSocket and starts the read loop. The provided ctx bounds the
// dial; the transport keeps an internal context for the connection lifetime.
func (t *WebSocketTransport) Connect(ctx context.Context) error {
	t.mu.Lock()
	if t.conn != nil {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()

	conn, _, err := websocket.Dial(ctx, t.url, t.opts)
	if err != nil {
		return err
	}
	// Lift the default 32KiB read limit; agent frames (stream_chunk state) can be large.
	conn.SetReadLimit(1 << 20)

	connCtx, cancel := context.WithCancel(context.Background())

	t.mu.Lock()
	t.conn = conn
	t.ctx = connCtx
	t.cancel = cancel
	t.mu.Unlock()

	go t.readLoop(connCtx, conn)
	return nil
}

func (t *WebSocketTransport) readLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.finish(err)
			return
		}
		select {
		case t.recv <- data:
		case <-ctx.Done():
			t.finish(ctx.Err())
			return
		}
	}
}

// Send writes a text frame.
func (t *WebSocketTransport) Send(frame []byte) error {
	t.mu.Lock()
	conn := t.conn
	closed := t.closed
	ctx := t.ctx
	t.mu.Unlock()
	if conn == nil || closed {
		return ErrTransportClosed
	}
	return conn.Write(ctx, websocket.MessageText, frame)
}

// Receive returns the inbound frame channel.
func (t *WebSocketTransport) Receive() <-chan []byte { return t.recv }

// Close shuts the connection down cleanly.
func (t *WebSocketTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	conn := t.conn
	cancel := t.cancel
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "client disconnect")
	}
	t.finish(nil)
	return nil
}

// Err returns the terminal error, if any.
func (t *WebSocketTransport) Err() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.err
}

// finish records the terminal error (first one wins) and closes the receive channel
// exactly once.
func (t *WebSocketTransport) finish(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	// A normal closure / context cancel is not surfaced as an error.
	if err != nil && !errors.Is(err, context.Canceled) &&
		websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.err = err
	}
	close(t.recv)
}
