using System.Collections.Concurrent;
using System.Diagnostics;
using System.Text.Json.Nodes;

namespace SmooAI.SmoothOperator.Core.Extensions;

/// <summary>Handles ext→host requests and notifications. The default answers <c>ping</c> and rejects
/// everything else with <c>MethodNotFound</c>; the host supplies a richer implementation once
/// ext→host methods (session/ui/kv/…) are wired.</summary>
public interface IInboundHandler
{
    Task<JsonNode> HandleRequestAsync(string method, JsonNode? @params);
    void HandleNotification(string method, JsonNode? @params);
}

/// <summary>The trivial handler: ping only. Used when the host wires nothing richer.</summary>
public sealed class DefaultInboundHandler : IInboundHandler
{
    public Task<JsonNode> HandleRequestAsync(string method, JsonNode? @params)
    {
        if (method == SepMethods.Ping)
        {
            return Task.FromResult<JsonNode>(new JsonObject());
        }
        throw new RpcException(SepCodes.MethodNotFound, $"method not found: {method}");
    }

    public void HandleNotification(string method, JsonNode? @params) { }
}

/// <summary>How to launch the subprocess. The manifest owns the full shape; this is just what
/// <see cref="ExtensionProcess.Spawn"/> needs.</summary>
public sealed class SpawnSpec
{
    public required string Command { get; init; }
    public List<string> Args { get; init; } = new();
    public Dictionary<string, string> Env { get; init; } = new();
    /// <summary>Working directory for the child (the extension's root).</summary>
    public string? Cwd { get; init; }
}

/// <summary>
/// One extension subprocess, its ndjson codec, and its request/response plumbing. Framing is
/// identical to MCP stdio: one JSON-RPC message per line on the child's stdin/stdout, stderr drained
/// to the host log. A reader task routes inbound responses to their pending caller and inbound
/// requests to an <see cref="IInboundHandler"/>; a writer task serializes outbound frames. Restart
/// (<see cref="Respawn"/>) bumps a generation counter so a stale reader from the dead child can't
/// resolve a request registered against the new child. Mirrors the Rust <c>ExtensionProcess</c>.
/// </summary>
public sealed class ExtensionProcess : IDisposable
{
    /// <summary>Backoff schedule for restart attempts. After the third failed attempt the host marks
    /// the extension failed and stops trying.</summary>
    public static readonly TimeSpan[] RestartBackoffs =
    {
        TimeSpan.FromSeconds(1), TimeSpan.FromSeconds(5), TimeSpan.FromSeconds(25),
    };

    /// <summary>Idle interval after which the host should health-probe with <c>ping</c>.</summary>
    public static readonly TimeSpan PingIdle = TimeSpan.FromSeconds(60);

    /// <summary>Bounded depth of the per-connection observe (<c>event</c>) lane. Past this, the OLDEST
    /// event is shed and an <c>events_lost</c> marker is delivered on recovery.</summary>
    public const int ObserveQueueCap = 1024;

    private readonly SpawnSpec _spec;
    private readonly IInboundHandler _handler;
    private readonly ConcurrentDictionary<long, TaskCompletionSource<JsonNode>> _pending = new();
    private long _generation;
    private long _nextId;
    private volatile bool _alive;
    private readonly object _connLock = new();
    private Connection _conn;

    private ExtensionProcess(SpawnSpec spec, IInboundHandler handler, Connection conn)
    {
        _spec = spec;
        _handler = handler;
        _conn = conn;
        _alive = true;
    }

    /// <summary>Backoff for restart <paramref name="attempt"/> (0-indexed), or null once attempts are
    /// exhausted (the caller transitions the extension to failed).</summary>
    public static TimeSpan? BackoffFor(int attempt) =>
        attempt >= 0 && attempt < RestartBackoffs.Length ? RestartBackoffs[attempt] : null;

    /// <summary>Spawn the subprocess and start its reader/writer tasks.</summary>
    public static ExtensionProcess Spawn(SpawnSpec spec, IInboundHandler handler)
    {
        // Placeholder; real connection is started after construction so tasks can reference `this`.
        var proc = new ExtensionProcess(spec, handler, Connection.Placeholder);
        proc._conn = proc.StartConnection(0);
        return proc;
    }

    private Connection StartConnection(long myGeneration)
    {
        var psi = new ProcessStartInfo
        {
            FileName = _spec.Command,
            RedirectStandardInput = true,
            RedirectStandardOutput = true,
            RedirectStandardError = true,
            UseShellExecute = false,
            WorkingDirectory = _spec.Cwd ?? Directory.GetCurrentDirectory(),
        };
        foreach (var arg in _spec.Args)
        {
            psi.ArgumentList.Add(arg);
        }
        foreach (var (k, v) in _spec.Env)
        {
            psi.Environment[k] = v;
        }

        Process child;
        try
        {
            child = Process.Start(psi) ?? throw new InvalidOperationException("Process.Start returned null");
        }
        catch (Exception e)
        {
            throw new InvalidOperationException($"spawn extension `{_spec.Command}`: {e.Message}", e);
        }

        var cts = new CancellationTokenSource();
        var conn = new Connection(child, cts);

        conn.Writer = Task.Run(() => WriterLoopAsync(child, conn, cts.Token));
        conn.Reader = Task.Run(() => ReaderLoopAsync(child, myGeneration, cts.Token));
        conn.StderrDrain = Task.Run(() => StderrLoopAsync(child, cts.Token));
        return conn;
    }

    private async Task WriterLoopAsync(Process child, Connection conn, CancellationToken ct)
    {
        var stdin = child.StandardInput;
        try
        {
            while (!ct.IsCancellationRequested)
            {
                await conn.WriteSignal.WaitAsync(ct).ConfigureAwait(false);
                // Control frames (requests/responses/cancel) always win; then drain the bounded
                // observe lane (events_lost marker first, if any).
                while (conn.Control.TryDequeue(out var frame))
                {
                    if (!await WriteFrameAsync(stdin, frame).ConfigureAwait(false))
                    {
                        return;
                    }
                }
                while (conn.Observe.PopForWrite() is { } evt)
                {
                    if (!await WriteFrameAsync(stdin, evt).ConfigureAwait(false))
                    {
                        return;
                    }
                }
            }
        }
        catch (OperationCanceledException)
        {
            // Connection torn down.
        }
    }

    private static async Task<bool> WriteFrameAsync(StreamWriter stdin, Message msg)
    {
        try
        {
            await stdin.WriteAsync(msg.ToJson() + "\n").ConfigureAwait(false);
            await stdin.FlushAsync().ConfigureAwait(false);
            return true;
        }
        catch
        {
            return false; // broken pipe → caller tears the connection down.
        }
    }

    private async Task ReaderLoopAsync(Process child, long myGeneration, CancellationToken ct)
    {
        var stdout = child.StandardOutput;
        try
        {
            string? line;
            while ((line = await stdout.ReadLineAsync(ct).ConfigureAwait(false)) is not null)
            {
                if (string.IsNullOrWhiteSpace(line))
                {
                    continue;
                }
                await DispatchLineAsync(line, myGeneration).ConfigureAwait(false);
            }
        }
        catch (OperationCanceledException)
        {
            return;
        }
        catch (Exception)
        {
            // Read error — treat as EOF below.
        }
        // Only the current generation's reader may declare death and fail pending.
        if (Interlocked.Read(ref _generation) == myGeneration)
        {
            _alive = false;
            FailAllPending("extension connection closed");
        }
    }

    private async Task DispatchLineAsync(string line, long myGeneration)
    {
        var msg = Message.TryParse(line);
        if (msg is null)
        {
            return; // unparseable frame — drop, matching Rust's warn+continue.
        }

        if (msg.IsResponse)
        {
            // Generation guard: drop responses that belong to a prior child.
            if (Interlocked.Read(ref _generation) != myGeneration)
            {
                return;
            }
            if (msg.Id is null || !msg.Id.AsValue().TryGetValue<long>(out var id))
            {
                return;
            }
            if (_pending.TryRemove(id, out var tcs))
            {
                if (msg.Error is not null)
                {
                    tcs.TrySetException(msg.Error.ToException());
                }
                else
                {
                    tcs.TrySetResult(msg.Result ?? new JsonObject());
                }
            }
        }
        else if (msg.IsRequest)
        {
            var id = msg.Id!;
            var method = msg.Method!;
            Message reply;
            try
            {
                var result = await _handler.HandleRequestAsync(method, msg.Params).ConfigureAwait(false);
                reply = Message.Success(id, result);
            }
            catch (RpcException rpc)
            {
                reply = Message.ErrorResponse(id, rpc.Error);
            }
            catch (Exception e)
            {
                reply = Message.ErrorResponse(id, new RpcError(SepCodes.InternalError, e.Message));
            }
            EnqueueControl(reply);
        }
        else if (msg.IsNotification)
        {
            _handler.HandleNotification(msg.Method!, msg.Params);
        }
    }

    private async Task StderrLoopAsync(Process child, CancellationToken ct)
    {
        try
        {
            var stderr = child.StandardError;
            string? line;
            while ((line = await stderr.ReadLineAsync(ct).ConfigureAwait(false)) is not null)
            {
                Debug.WriteLine($"ext[{_spec.Command}] stderr: {line}");
            }
        }
        catch
        {
            // Draining stderr is best-effort.
        }
    }

    private void EnqueueControl(Message frame)
    {
        lock (_connLock)
        {
            _conn.Control.Enqueue(frame);
            _conn.WriteSignal.Release();
        }
    }

    /// <summary>Send a request and await its response, bounded by <paramref name="timeout"/>. On
    /// timeout or cancellation the peer is told to stop via <c>$/cancel</c> and the pending slot is
    /// cleared.</summary>
    /// <exception cref="RpcException">The extension replied with a JSON-RPC error, or the connection is dead.</exception>
    /// <exception cref="TimeoutException">The request timed out.</exception>
    public async Task<JsonNode> RequestAsync(string method, JsonNode? @params, TimeSpan timeout, CancellationToken cancellationToken = default)
    {
        if (!_alive)
        {
            throw new RpcException(SepCodes.InternalError, "extension is not alive");
        }
        var id = Interlocked.Increment(ref _nextId);
        var tcs = new TaskCompletionSource<JsonNode>(TaskCreationOptions.RunContinuationsAsynchronously);
        _pending[id] = tcs;

        EnqueueControl(Message.Request(JsonValue.Create(id), method, @params));

        using var timeoutCts = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken);
        timeoutCts.CancelAfter(timeout);
        try
        {
            return await tcs.Task.WaitAsync(timeoutCts.Token).ConfigureAwait(false);
        }
        catch (OperationCanceledException)
        {
            // Timeout or external cancel: clear the pending slot and tell the peer to stop.
            _pending.TryRemove(id, out _);
            TryCancel(id);
            if (cancellationToken.IsCancellationRequested)
            {
                throw;
            }
            throw new TimeoutException($"extension request `{method}` timed out after {timeout}");
        }
    }

    /// <summary>Best-effort <c>$/cancel</c> for an in-flight request id. The peer SHOULD stop and
    /// reply with -32800 Cancelled; a cancel for an already-answered id is a harmless no-op.</summary>
    public void TryCancel(long id) => Notify(SepMethods.Cancel, new JsonObject { ["id"] = id });

    /// <summary>Send a fire-and-forget notification on the reliable control lane.</summary>
    public void Notify(string method, JsonNode? @params) => EnqueueControl(Message.Notification(method, @params));

    /// <summary>Enqueue an observe <c>event</c> on the bounded, lossy lane. Sheds the oldest queued
    /// event (tracked for the next <c>events_lost</c> marker) rather than block or grow unbounded when
    /// the extension is not draining its stdin. Never fails.</summary>
    public void SendEvent(string @event, JsonNode context, JsonNode? payload)
    {
        ObserveLane observe;
        lock (_connLock)
        {
            observe = _conn.Observe;
        }
        observe.Push(@event, context, payload);
        lock (_connLock)
        {
            _conn.WriteSignal.Release();
        }
    }

    public bool IsAlive => _alive;

    public long Generation => Interlocked.Read(ref _generation);

    /// <summary>Health-probe with <c>ping</c>. True if the extension answered within the timeout.</summary>
    public async Task<bool> PingHealthAsync(TimeSpan timeout)
    {
        try
        {
            await RequestAsync(SepMethods.Ping, new JsonObject(), timeout).ConfigureAwait(false);
            return true;
        }
        catch
        {
            return false;
        }
    }

    /// <summary>Kill and re-spawn the child in place. Bumps the generation (invalidating any stale
    /// reader and failing every in-flight request), then starts a fresh connection. <c>_nextId</c> is
    /// NOT reset, so ids never collide across generations.</summary>
    public void Respawn()
    {
        var newGeneration = Interlocked.Increment(ref _generation);
        FailAllPending("extension restarting");
        Connection old;
        lock (_connLock)
        {
            old = _conn;
        }
        old.Abort();
        var newConn = StartConnection(newGeneration);
        _alive = true;
        lock (_connLock)
        {
            _conn = newConn;
        }
    }

    /// <summary>Graceful shutdown: send <c>shutdown</c>, wait up to <paramref name="grace"/> for the
    /// reply, then force-kill. Always leaves the process dead.</summary>
    public async Task ShutdownAsync(TimeSpan grace)
    {
        try
        {
            await RequestAsync(SepMethods.Shutdown, new JsonObject(), grace).ConfigureAwait(false);
        }
        catch
        {
            // best-effort
        }
        _alive = false;
        Connection conn;
        lock (_connLock)
        {
            conn = _conn;
        }
        conn.Abort();
    }

    private void FailAllPending(string reason)
    {
        foreach (var key in _pending.Keys.ToArray())
        {
            if (_pending.TryRemove(key, out var tcs))
            {
                tcs.TrySetException(new RpcException(SepCodes.InternalError, reason));
            }
        }
    }

    public void Dispose()
    {
        Connection conn;
        lock (_connLock)
        {
            conn = _conn;
        }
        conn.Abort();
    }

    /// <summary>A live child connection: the writer queue + signal, the observe lane, the process, and
    /// the cancellation that stops its reader/writer/stderr tasks. Replaced wholesale on respawn.</summary>
    private sealed class Connection
    {
        public static readonly Connection Placeholder = new(null, null);

        public Process? Child { get; }
        public CancellationTokenSource? Cts { get; }
        public ConcurrentQueue<Message> Control { get; } = new();
        public SemaphoreSlim WriteSignal { get; } = new(0);
        public ObserveLane Observe { get; } = new();
        public Task? Writer { get; set; }
        public Task? Reader { get; set; }
        public Task? StderrDrain { get; set; }

        public Connection(Process? child, CancellationTokenSource? cts)
        {
            Child = child;
            Cts = cts;
        }

        public void Abort()
        {
            try
            {
                Cts?.Cancel();
            }
            catch
            {
                // ignore
            }
            try
            {
                if (Child is { HasExited: false })
                {
                    Child.Kill(entireProcessTree: true);
                }
            }
            catch
            {
                // child may already be gone
            }
        }
    }
}

/// <summary>The per-connection observe lane: a bounded, oldest-shedding queue of <c>event</c> frames
/// plus a monotonic sequence and a shed counter. Fire-and-forget events go here so a stuck child
/// stdin can't grow host memory without bound. Mirrors the Rust <c>ObserveLane</c>.</summary>
internal sealed class ObserveLane
{
    private readonly object _lock = new();
    private readonly Queue<Message> _queue = new();
    private long _seq;
    private long _lost;
    private JsonNode _lastContext = new JsonObject();

    public long Seq => Interlocked.Read(ref _seq);
    public long Lost => Interlocked.Read(ref _lost);
    public int QueueCount { get { lock (_lock) { return _queue.Count; } } }

    public void Push(string @event, JsonNode context, JsonNode? payload)
    {
        var seq = Interlocked.Increment(ref _seq) - 1;
        var frame = Message.Notification(SepMethods.Event, new JsonObject
        {
            ["event"] = @event,
            ["seq"] = seq,
            ["context"] = context.DeepClone(),
            ["payload"] = payload?.DeepClone(),
        });
        lock (_lock)
        {
            if (_queue.Count >= ExtensionProcess.ObserveQueueCap)
            {
                _queue.Dequeue();
                Interlocked.Increment(ref _lost);
            }
            _queue.Enqueue(frame);
            _lastContext = context.DeepClone();
        }
    }

    /// <summary>Next frame to flush, or null when drained. Emits an <c>events_lost</c> marker (no seq —
    /// out-of-band; a gap in the seq run signals the loss, the marker carries the exact count) before
    /// the surviving events whenever shedding happened since the last drain.</summary>
    public Message? PopForWrite()
    {
        lock (_lock)
        {
            var lost = Interlocked.Exchange(ref _lost, 0);
            if (lost > 0)
            {
                return Message.Notification(SepMethods.Event, new JsonObject
                {
                    ["event"] = SepEvents.EventsLost,
                    ["context"] = _lastContext.DeepClone(),
                    ["payload"] = new JsonObject { ["lost"] = lost },
                });
            }
            return _queue.Count > 0 ? _queue.Dequeue() : null;
        }
    }
}
