/**
 * `ExtensionProcess` — one extension subprocess, its ndjson codec, and its
 * request/response plumbing. The TS sibling of the Rust host's
 * `extension/process.rs`.
 *
 * Framing is identical to MCP stdio: one JSON-RPC message per line on the child's
 * stdin/stdout, stderr drained to host logging. Inbound responses route to their
 * pending caller; inbound requests go to an {@link InboundHandler}.
 *
 * Restart is in-place ({@link ExtensionProcess.respawn}): a generation counter is
 * bumped so a stale reader from the dead child can't resolve a request registered
 * against the new child, and every in-flight request fails fast.
 */

import { type ChildProcessWithoutNullStreams, spawn } from 'node:child_process';
import { createInterface, type Interface as ReadlineInterface } from 'node:readline';
import { codes, type Id, isNotification, isRequest, isResponse, type Message, notification, request, RpcError, type RpcErrorObject, success, errorResponse } from './protocol.js';

/**
 * Backoff schedule (ms) for restart attempts. After the third failed attempt the
 * host marks the extension failed and stops trying.
 */
export const RESTART_BACKOFFS_MS: readonly number[] = [1000, 5000, 25000];

/** Idle interval (ms) after which the host should health-probe with `ping`. */
export const PING_IDLE_MS = 60_000;

/**
 * Bounded depth of the per-connection observe (`event`) lane. When a slow or
 * stalled extension lets events pile past this, the OLDEST are shed and an
 * `events_lost` marker is delivered on recovery — observe events are lossy by
 * contract. Requests (hook/tool/ping/shutdown) are NEVER shed; they ride the
 * reliable control lane (a direct stdin write).
 */
export const OBSERVE_QUEUE_CAP = 1024;

/**
 * Backoff (ms) for restart `attempt` (0-indexed). `undefined` once attempts are
 * exhausted — the caller transitions the extension to failed.
 */
export function backoffFor(attempt: number): number | undefined {
    return RESTART_BACKOFFS_MS[attempt];
}

/**
 * Handles ext→host requests and notifications. The default answers `ping` and
 * rejects everything else with MethodNotFound; the host supplies a richer impl
 * once ext→host methods (session/ui/kv/…) are wired.
 */
export interface InboundHandler {
    handleRequest(method: string, params: unknown): Promise<unknown>;
    handleNotification(method: string, params: unknown): void;
}

/** The trivial handler: ping only. Used when the host wires nothing richer. */
export class DefaultInboundHandler implements InboundHandler {
    async handleRequest(method: string, _params: unknown): Promise<unknown> {
        if (method === 'ping') return {};
        throw new RpcError(codes.MethodNotFound, `method not found: ${method}`);
    }
    handleNotification(_method: string, _params: unknown): void {}
}

/** How to launch the subprocess. The manifest owns the full shape; this is what `spawn` needs. */
export interface SpawnSpec {
    command: string;
    args: string[];
    env: Record<string, string>;
    /** Working directory for the child (the extension's root). */
    cwd?: string;
}

interface Pending {
    resolve: (value: unknown) => void;
    reject: (err: Error) => void;
    timer: ReturnType<typeof setTimeout>;
}

/**
 * The per-connection observe lane: a bounded, oldest-shedding queue of `event`
 * frames plus a monotonic sequence and a shed counter. Fire-and-forget events go
 * here so a stuck child stdin can't grow host memory without bound.
 */
class ObserveLane {
    private readonly queue: Message[] = [];
    private seq = 0;
    private lost = 0;
    private lastContext: unknown = null;

    /** Enqueue an `event` frame, shedding the oldest if at capacity. */
    push(event: string, context: unknown, payload: unknown): void {
        const seq = this.seq++;
        const frame = notification('event', { event, seq, context, payload });
        if (this.queue.length >= OBSERVE_QUEUE_CAP) {
            this.queue.shift();
            this.lost++;
        }
        this.queue.push(frame);
        this.lastContext = context;
    }

    /**
     * Next frame for the writer to flush, or `undefined` when drained. Emits an
     * `events_lost` marker (no `seq`) before the surviving events whenever
     * shedding happened since the last drain — a gap in the seq run signals the
     * loss; the marker carries the exact count.
     */
    popForWrite(): Message | undefined {
        if (this.lost > 0) {
            const lost = this.lost;
            this.lost = 0;
            return notification('event', { event: 'events_lost', context: this.lastContext, payload: { lost } });
        }
        return this.queue.shift();
    }

    get length(): number {
        return this.queue.length;
    }
    get lostCount(): number {
        return this.lost;
    }
    get seqValue(): number {
        return this.seq;
    }
}

/** A live child connection: the process, its reader, and the observe lane. */
interface Connection {
    child: ChildProcessWithoutNullStreams;
    rl: ReadlineInterface;
    observe: ObserveLane;
    /** True once the observe pump has been scheduled and is draining. */
    pumping: boolean;
}

/** One extension subprocess. */
export class ExtensionProcess {
    private readonly pending = new Map<number, Pending>();
    private generation = 0;
    private nextId = 1;
    private alive = true;
    private conn: Connection;

    private constructor(
        private readonly spec: SpawnSpec,
        private readonly handler: InboundHandler,
    ) {
        this.conn = this.startConnection(0);
    }

    /** Spawn the subprocess and start its reader. Throws if it can't be spawned. */
    static spawn(spec: SpawnSpec, handler: InboundHandler): ExtensionProcess {
        return new ExtensionProcess(spec, handler);
    }

    private startConnection(myGeneration: number): Connection {
        const child = spawn(this.spec.command, this.spec.args, {
            cwd: this.spec.cwd,
            env: { ...process.env, ...this.spec.env },
            stdio: ['pipe', 'pipe', 'pipe'],
        }) as ChildProcessWithoutNullStreams;
        child.on('error', (err) => {
            if (this.generation === myGeneration) {
                this.alive = false;
                this.failAllPending(`extension spawn error: ${err.message}`);
            }
        });

        const rl = createInterface({ input: child.stdout, terminal: false });
        rl.on('line', (line) => {
            if (!line.trim()) return;
            this.dispatchLine(line, myGeneration);
        });

        const errRl = createInterface({ input: child.stderr, terminal: false });
        errRl.on('line', (line) => {
            console.error(`[ext ${this.spec.command}] ${line}`);
        });

        const onClose = (): void => {
            // Only the current generation's reader may declare death and fail
            // pending — a stale reader must not disturb a fresh child.
            if (this.generation === myGeneration) {
                this.alive = false;
                this.failAllPending('extension connection closed');
            }
        };
        child.stdout.on('close', onClose);
        child.on('exit', onClose);

        return { child, rl, observe: new ObserveLane(), pumping: false };
    }

    /** Parse and route one inbound line. */
    private dispatchLine(line: string, myGeneration: number): void {
        let msg: Message;
        try {
            msg = JSON.parse(line) as Message;
        } catch {
            console.error(`[ext ${this.spec.command}] unparseable frame: ${line}`);
            return;
        }

        if (isResponse(msg)) {
            // Generation guard: drop responses that belong to a prior child.
            if (this.generation !== myGeneration) return;
            const id = typeof msg.id === 'number' ? msg.id : undefined;
            if (id === undefined) return;
            const pending = this.pending.get(id);
            if (!pending) return;
            this.pending.delete(id);
            clearTimeout(pending.timer);
            if (msg.error) pending.reject(new RpcError(msg.error.code, msg.error.message, msg.error.data));
            else pending.resolve(msg.result ?? null);
        } else if (isRequest(msg)) {
            void this.handleInboundRequest(msg.id ?? null, msg.method ?? '', msg.params);
        } else if (isNotification(msg)) {
            this.handler.handleNotification(msg.method ?? '', msg.params);
        }
    }

    private async handleInboundRequest(id: Id, method: string, params: unknown): Promise<void> {
        let reply: Message;
        try {
            reply = success(id, (await this.handler.handleRequest(method, params)) ?? {});
        } catch (err) {
            const obj: RpcErrorObject =
                err instanceof RpcError ? err.toObject() : { code: codes.InternalError, message: err instanceof Error ? err.message : String(err) };
            reply = errorResponse(id, obj);
        }
        this.writeFrame(reply);
    }

    /**
     * Send a request and await its response, bounded by `timeoutMs`. Rejects if
     * the connection is dead, the request times out (it also sends `$/cancel`), or
     * the extension replies with a JSON-RPC error.
     */
    request(method: string, params: unknown, timeoutMs: number): Promise<unknown> {
        if (!this.alive) return Promise.reject(new Error('extension is not alive'));
        const id = this.nextId++;
        return new Promise<unknown>((resolve, reject) => {
            const timer = setTimeout(() => {
                // Timed out: clear the slot, tell the peer to stop (best-effort
                // `$/cancel`), and fail the caller — mirrors the Rust CancelGuard.
                if (this.pending.delete(id)) {
                    this.cancel(id);
                    reject(new Error(`extension request \`${method}\` timed out after ${timeoutMs}ms`));
                }
            }, timeoutMs);
            this.pending.set(id, { resolve, reject, timer });
            if (!this.writeFrame(request(id, method, params))) {
                if (this.pending.delete(id)) {
                    clearTimeout(timer);
                    reject(new Error('extension writer is gone'));
                }
            }
        });
    }

    /**
     * Best-effort `$/cancel` for an in-flight request `id`. The peer SHOULD stop
     * work; a cancel for an already-answered id is a harmless no-op.
     */
    cancel(id: number): void {
        this.notify('$/cancel', { id });
    }

    /** Send a fire-and-forget notification on the reliable control lane. */
    notify(method: string, params: unknown): void {
        this.writeFrame(notification(method, params));
    }

    /**
     * Enqueue an observe `event` on the bounded, lossy lane. Assigns the frame a
     * per-connection sequence; sheds the oldest queued event (tracked for the next
     * `events_lost` marker) rather than block or grow unbounded when the extension
     * is not draining its stdin. Never throws — a shed event is the contract.
     */
    sendEvent(event: string, context: unknown, payload: unknown): void {
        this.conn.observe.push(event, context, payload);
        this.pumpObserve();
    }

    /** Whether the connection is currently believed alive. */
    isAlive(): boolean {
        return this.alive;
    }

    /** Current generation (increments on every successful respawn). */
    getGeneration(): number {
        return this.generation;
    }

    /** Health-probe with `ping`; resolves `true` if answered within `timeoutMs`. */
    async pingHealth(timeoutMs: number): Promise<boolean> {
        try {
            await this.request('ping', {}, timeoutMs);
            return true;
        } catch {
            return false;
        }
    }

    /**
     * Kill and re-spawn the child in place. Bumps the generation (invalidating any
     * stale reader and failing every in-flight request), then starts a fresh
     * connection. `nextId` is NOT reset, so ids never collide across generations.
     */
    respawn(): void {
        this.generation++;
        const myGeneration = this.generation;
        this.failAllPending('extension restarting');
        this.abortConnection();
        this.conn = this.startConnection(myGeneration);
        this.alive = true;
    }

    /**
     * Graceful shutdown: send `shutdown`, wait up to `graceMs` for the reply, then
     * force-kill. Always leaves the process dead.
     */
    async shutdown(graceMs: number): Promise<void> {
        try {
            await this.request('shutdown', {}, graceMs);
        } catch {
            // Ignore — we kill regardless below.
        }
        this.alive = false;
        this.abortConnection();
    }

    /** Serialize a frame as ndjson to the child stdin. Returns false on any error. */
    private writeFrame(msg: Message): boolean {
        let line: string;
        try {
            line = `${JSON.stringify(msg)}\n`;
        } catch (e) {
            console.error(`[ext ${this.spec.command}] failed to serialize frame: ${String(e)}`);
            return true; // a bad frame is not a broken pipe — keep the connection.
        }
        const stdin = this.conn.child.stdin;
        if (!stdin.writable) return false;
        stdin.write(line);
        return true;
    }

    /** Drain the observe lane to stdin, honoring backpressure via `drain`. */
    private pumpObserve(): void {
        const conn = this.conn;
        if (conn.pumping) return;
        conn.pumping = true;
        const stdin = conn.child.stdin;
        const drain = (): void => {
            for (;;) {
                const frame = conn.observe.popForWrite();
                if (!frame) {
                    conn.pumping = false;
                    return;
                }
                if (!stdin.writable) {
                    conn.pumping = false;
                    return;
                }
                const ok = stdin.write(`${JSON.stringify(frame)}\n`);
                if (!ok) {
                    // Backpressure: wait for the child to drain, then continue. The
                    // observe queue keeps shedding oldest meanwhile (bounded memory).
                    stdin.once('drain', drain);
                    return;
                }
            }
        };
        drain();
    }

    /** Fail every pending request with the same error message. */
    private failAllPending(reason: string): void {
        const drained = [...this.pending.values()];
        this.pending.clear();
        for (const p of drained) {
            clearTimeout(p.timer);
            p.reject(new RpcError(codes.InternalError, reason));
        }
    }

    /** Tear down the current connection's child + readers. */
    private abortConnection(): void {
        const { child, rl } = this.conn;
        rl.close();
        child.stdout.removeAllListeners();
        child.stderr.removeAllListeners();
        child.removeAllListeners('exit');
        try {
            child.kill('SIGKILL');
        } catch {
            // already gone
        }
    }
}
