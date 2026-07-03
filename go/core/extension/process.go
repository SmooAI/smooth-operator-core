package extension

// ExtensionProcess — one extension subprocess, its ndjson codec, and its
// request/response plumbing.
//
// Framing is identical to MCP stdio: one JSON-RPC message per line on the
// child's stdin/stdout, stderr drained so the pipe can't fill. A reader
// goroutine routes inbound responses to their pending caller and inbound
// requests to an InboundHandler; a writer goroutine serializes outbound frames.
//
// Restart is in-place (Respawn): a generation counter is bumped so a stale
// reader from the dead child can't resolve a request registered against the new
// child, and every in-flight request fails fast.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// RestartBackoffs is the backoff schedule for restart attempts. After the third
// failed attempt the host marks the extension failed and stops trying.
var RestartBackoffs = [3]time.Duration{1 * time.Second, 5 * time.Second, 25 * time.Second}

// PingIdle is the idle interval after which the host should health-probe with ping.
const PingIdle = 60 * time.Second

// ObserveQueueCap is the bounded depth of the per-connection observe (event)
// lane. When a slow extension lets events pile past this, the OLDEST are shed and
// an events_lost marker is delivered on recovery — observe events are lossy by
// contract. Requests (hook/tool/ping/shutdown) are NEVER shed; they ride the
// reliable control lane.
const ObserveQueueCap = 1024

// BackoffFor returns the backoff for restart attempt (0-indexed) and whether
// attempts remain. When false, the caller transitions the extension to failed.
func BackoffFor(attempt int) (time.Duration, bool) {
	if attempt < 0 || attempt >= len(RestartBackoffs) {
		return 0, false
	}
	return RestartBackoffs[attempt], true
}

// InboundHandler handles ext→host requests and notifications. HandleRequest may
// block (a ui/confirm bridge parks until a human answers); the process runs each
// inbound request in its own goroutine so a slow handler never stalls the reader.
type InboundHandler interface {
	HandleRequest(method string, params json.RawMessage) (json.RawMessage, *RpcError)
	HandleNotification(method string, params json.RawMessage)
}

// DefaultInboundHandler answers ping and rejects everything else with
// MethodNotFound. Used when the host wires nothing richer.
type DefaultInboundHandler struct{}

func (DefaultInboundHandler) HandleRequest(method string, _ json.RawMessage) (json.RawMessage, *RpcError) {
	if method == MethodPing {
		return json.RawMessage("{}"), nil
	}
	return nil, NewRpcError(CodeMethodNotFound, "method not found: "+method)
}

func (DefaultInboundHandler) HandleNotification(string, json.RawMessage) {}

// SpawnSpec is how to launch the subprocess.
type SpawnSpec struct {
	Command string
	Args    []string
	Env     map[string]string
	// Cwd is the working directory for the child (the extension's root); "" inherits.
	Cwd string
}

type pendingResult struct {
	value json.RawMessage
	err   *RpcError
}

// eventFrameParams is the wire shape of an event notification's params.
type eventFrameParams struct {
	Event   string          `json:"event"`
	Seq     *uint64         `json:"seq,omitempty"`
	Context json.RawMessage `json:"context"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// observeLane is the per-connection observe lane: a bounded, oldest-shedding
// queue of event frames plus a monotonic sequence and a shed counter.
type observeLane struct {
	mu          sync.Mutex
	queue       []Message
	seq         uint64
	lost        uint64
	lastContext json.RawMessage
	signal      chan struct{}
}

func newObserveLane() *observeLane {
	return &observeLane{signal: make(chan struct{}, 1)}
}

func (l *observeLane) push(event string, ctx, payload json.RawMessage) {
	l.mu.Lock()
	seq := l.seq
	l.seq++
	params, _ := json.Marshal(eventFrameParams{Event: event, Seq: &seq, Context: ctx, Payload: payload})
	frame := NewNotification(MethodEvent, params)
	if len(l.queue) >= ObserveQueueCap {
		l.queue = l.queue[1:]
		l.lost++
	}
	l.queue = append(l.queue, frame)
	l.lastContext = ctx
	l.mu.Unlock()
	select {
	case l.signal <- struct{}{}:
	default:
	}
}

// popForWrite returns the next frame for the writer to flush, or ok=false when
// drained. It emits an events_lost marker (no seq) before the surviving events
// whenever shedding happened since the last drain.
func (l *observeLane) popForWrite() (Message, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lost > 0 {
		lost := l.lost
		l.lost = 0
		params, _ := json.Marshal(eventFrameParams{
			Event:   "events_lost",
			Context: l.lastContext,
			Payload: json.RawMessage(`{"lost":` + strconv.FormatUint(lost, 10) + `}`),
		})
		return NewNotification(MethodEvent, params), true
	}
	if len(l.queue) == 0 {
		return Message{}, false
	}
	f := l.queue[0]
	l.queue = l.queue[1:]
	return f, true
}

// connection is a live child connection: the control lane plus the child handle
// and the teardown signal. Replaced wholesale on Respawn.
//
// ponytail: control lane is a buffered channel (cap 256), not the Rust
// unbounded mpsc — the host sends serially per extension (hooks chain, tools are
// non-concurrent) so it never fills; a genuinely stalled child is reaped by the
// request timeout / ping health. Upgrade to a mutex+slice queue if a real
// workload ever bursts control frames faster than the child drains stdin.
type connection struct {
	outbound  chan Message
	observe   *observeLane
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	done      chan struct{}
	closeOnce sync.Once
}

func (c *connection) abort() {
	c.closeOnce.Do(func() { close(c.done) })
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

// send enqueues a control frame, returning false if the connection is torn down.
func (c *connection) send(m Message) bool {
	select {
	case c.outbound <- m:
		return true
	case <-c.done:
		return false
	}
}

// ExtensionProcess is one extension subprocess.
type ExtensionProcess struct {
	spec    SpawnSpec
	handler InboundHandler

	pendingMu sync.Mutex
	pending   map[uint64]chan pendingResult

	generation atomic.Uint64
	nextID     atomic.Uint64
	alive      atomic.Bool

	connMu sync.Mutex
	conn   *connection
}

// Spawn spawns the subprocess and starts its reader/writer goroutines.
func Spawn(spec SpawnSpec, handler InboundHandler) (*ExtensionProcess, error) {
	if handler == nil {
		handler = DefaultInboundHandler{}
	}
	p := &ExtensionProcess{
		spec:    spec,
		handler: handler,
		pending: map[uint64]chan pendingResult{},
	}
	p.alive.Store(true)
	conn, err := p.startConnection(0)
	if err != nil {
		return nil, err
	}
	p.conn = conn
	return p, nil
}

// startConnection spawns the child and wires the reader/writer/stderr goroutines
// for one generation. Shared by Spawn and Respawn.
func (p *ExtensionProcess) startConnection(myGeneration uint64) (*connection, error) {
	cmd := exec.Command(p.spec.Command, p.spec.Args...)
	cmd.Env = os.Environ()
	for k, v := range p.spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if p.spec.Cwd != "" {
		cmd.Dir = p.spec.Cwd
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	conn := &connection{
		outbound: make(chan Message, 256),
		observe:  newObserveLane(),
		cmd:      cmd,
		stdin:    stdin,
		done:     make(chan struct{}),
	}

	// Writer goroutine: control frames always win; the bounded observe lane is
	// drained after each control write and whenever it signals.
	go func() {
		for {
			select {
			case m := <-conn.outbound:
				if !writeFrame(stdin, m) {
					conn.abort()
					return
				}
				drainObserve(stdin, conn)
			case <-conn.observe.signal:
				if !drainObserve(stdin, conn) {
					conn.abort()
					return
				}
			case <-conn.done:
				return
			}
		}
	}()

	// stderr drain — keep the pipe from filling (the child would block otherwise).
	go func() {
		s := bufio.NewScanner(stderr)
		s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for s.Scan() {
			// ponytail: discard. Wire to a logger seam if extension diagnostics
			// ever need surfacing; today the pipe just needs draining.
		}
	}()

	// Reader goroutine: child stdout → route responses/requests.
	go func() {
		r := bufio.NewReader(stdout)
		for {
			line, err := r.ReadString('\n')
			if len(line) > 0 {
				p.dispatchLine(line, myGeneration, conn)
			}
			if err != nil {
				break
			}
		}
		// Reap the child (release the process handle) now that stdout is at EOF.
		conn.abort()
		_ = cmd.Wait()
		// Only the current generation's reader may declare death and fail pending.
		if p.generation.Load() == myGeneration {
			p.alive.Store(false)
			p.failAllPending("extension connection closed")
		}
	}()

	return conn, nil
}

// dispatchLine parses and routes one inbound line.
func (p *ExtensionProcess) dispatchLine(line string, myGeneration uint64, conn *connection) {
	var msg Message
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return // unparseable frame — drop.
	}

	switch {
	case msg.IsResponse():
		// Generation guard: drop responses that belong to a prior child.
		if p.generation.Load() != myGeneration {
			return
		}
		var id uint64
		if err := json.Unmarshal(msg.ID, &id); err != nil {
			return
		}
		ch := p.takePending(id)
		if ch == nil {
			return
		}
		if msg.Error != nil {
			ch <- pendingResult{err: msg.Error}
		} else {
			result := msg.Result
			if len(result) == 0 {
				result = json.RawMessage("null")
			}
			ch <- pendingResult{value: result}
		}
	case msg.IsRequest():
		id := msg.ID
		method := msg.Method
		params := msg.Params
		// Run the handler in its own goroutine so a slow ext→host request (e.g. a
		// parked ui/confirm) never blocks the reader from reading later frames.
		go func() {
			result, rpcErr := p.handler.HandleRequest(method, params)
			var reply Message
			if rpcErr != nil {
				reply = NewErrorResponse(id, rpcErr)
			} else {
				reply = NewSuccess(id, result)
			}
			conn.send(reply)
		}()
	case msg.IsNotification():
		p.handler.HandleNotification(msg.Method, msg.Params)
	}
}

// Request sends a request and awaits its response, bounded by timeout and ctx.
func (p *ExtensionProcess) Request(ctx context.Context, method string, params json.RawMessage, timeout time.Duration) (json.RawMessage, error) {
	if !p.alive.Load() {
		return nil, errors.New("extension is not alive")
	}
	id := p.nextID.Add(1)
	ch := make(chan pendingResult, 1)
	p.pendingMu.Lock()
	p.pending[id] = ch
	p.pendingMu.Unlock()

	conn := p.currentConn()
	if conn == nil || !conn.send(NewRequest(json.RawMessage(strconv.FormatUint(id, 10)), method, params)) {
		p.takePending(id)
		return nil, errors.New("extension writer is gone")
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		return res.value, nil
	case <-timer.C:
		p.takePending(id)
		p.cancel(id) // best-effort $/cancel so the peer stops working on it.
		return nil, errors.New("extension request " + method + " timed out")
	case <-ctx.Done():
		p.takePending(id)
		p.cancel(id)
		return nil, ctx.Err()
	}
}

// takePending removes and returns the pending channel for id, or nil if absent.
func (p *ExtensionProcess) takePending(id uint64) chan pendingResult {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	ch, ok := p.pending[id]
	if !ok {
		return nil
	}
	delete(p.pending, id)
	return ch
}

func (p *ExtensionProcess) failAllPending(reason string) {
	p.pendingMu.Lock()
	drained := p.pending
	p.pending = map[uint64]chan pendingResult{}
	p.pendingMu.Unlock()
	for _, ch := range drained {
		ch <- pendingResult{err: NewRpcError(CodeInternalError, reason)}
	}
}

func (p *ExtensionProcess) currentConn() *connection {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	return p.conn
}

// cancel sends a best-effort $/cancel for an in-flight request id.
func (p *ExtensionProcess) cancel(id uint64) {
	p.Notify(MethodCancel, json.RawMessage(`{"id":`+strconv.FormatUint(id, 10)+`}`))
}

// Notify sends a fire-and-forget notification.
func (p *ExtensionProcess) Notify(method string, params json.RawMessage) {
	if conn := p.currentConn(); conn != nil {
		conn.send(NewNotification(method, params))
	}
}

// SendEvent enqueues an observe event on the bounded, lossy lane. Never fails —
// a shed event is the contract, not an error.
func (p *ExtensionProcess) SendEvent(event string, ctx, payload json.RawMessage) {
	if conn := p.currentConn(); conn != nil {
		conn.observe.push(event, ctx, payload)
	}
}

// IsAlive reports whether the connection is currently believed alive.
func (p *ExtensionProcess) IsAlive() bool { return p.alive.Load() }

// Generation returns the current generation (increments on every Respawn).
func (p *ExtensionProcess) Generation() uint64 { return p.generation.Load() }

// PingHealth health-probes with ping. Returns true if the extension answered
// within timeout.
func (p *ExtensionProcess) PingHealth(ctx context.Context, timeout time.Duration) bool {
	_, err := p.Request(ctx, MethodPing, json.RawMessage("{}"), timeout)
	return err == nil
}

// Respawn kills and re-spawns the child in place. Bumps the generation
// (invalidating any stale reader and failing every in-flight request), then
// starts a fresh connection. nextID is NOT reset, so ids never collide across
// generations.
func (p *ExtensionProcess) Respawn() error {
	p.generation.Add(1)
	newGen := p.generation.Load()
	p.failAllPending("extension restarting")

	p.connMu.Lock()
	old := p.conn
	p.connMu.Unlock()
	if old != nil {
		old.abort()
	}

	newConn, err := p.startConnection(newGen)
	if err != nil {
		return err
	}
	p.alive.Store(true)
	p.connMu.Lock()
	p.conn = newConn
	p.connMu.Unlock()
	return nil
}

// Shutdown gracefully stops the extension: send shutdown, wait up to grace for
// the reply, then force-kill. Always leaves the process dead.
func (p *ExtensionProcess) Shutdown(ctx context.Context, grace time.Duration) {
	_, _ = p.Request(ctx, MethodShutdown, json.RawMessage("{}"), grace)
	p.alive.Store(false)
	if conn := p.currentConn(); conn != nil {
		conn.abort()
	}
}

// Close tears down the current connection (kills the child, stops goroutines).
// Idempotent; safe to defer in callers/tests.
func (p *ExtensionProcess) Close() {
	p.alive.Store(false)
	if conn := p.currentConn(); conn != nil {
		conn.abort()
	}
}

// drainObserve flushes the observe lane (events_lost marker first). Returns false
// on a write error so the writer can tear down.
func drainObserve(stdin io.Writer, conn *connection) bool {
	for {
		msg, ok := conn.observe.popForWrite()
		if !ok {
			return true
		}
		if !writeFrame(stdin, msg) {
			return false
		}
	}
}

// writeFrame serializes a frame as ndjson to the child stdin. Returns false on a
// write error (the caller tears the connection down). A serialization error is
// not a broken pipe — the frame is dropped and the connection kept.
func writeFrame(stdin io.Writer, msg Message) bool {
	line, err := json.Marshal(msg)
	if err != nil {
		return true
	}
	line = append(line, '\n')
	_, err = stdin.Write(line)
	return err == nil
}
