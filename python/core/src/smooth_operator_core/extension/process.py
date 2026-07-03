"""``ExtensionProcess`` — one extension subprocess, its ndjson codec, and its
request/response plumbing.

Python (asyncio) sibling of the Rust reference ``process.rs``. Framing is identical
to MCP stdio: one JSON-RPC message per line on the child's stdin/stdout, stderr
drained to host logging. A reader task routes inbound responses to their pending
caller and inbound requests to an :class:`InboundHandler`; a writer task serializes
outbound frames (a reliable control lane always winning over a bounded, lossy
observe lane).

Restart is in-place (:meth:`ExtensionProcess.respawn`): a generation counter is
bumped so a stale reader from the dead child can't resolve a request registered
against the new child, and every in-flight request fails fast.
"""

from __future__ import annotations

import asyncio
import json
import logging
import os
from collections import deque
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Optional

from .protocol import Message, RpcError, codes, method

_log = logging.getLogger("smooth_operator_core.extension")

#: Backoff schedule (seconds) for restart attempts. After the third failed attempt
#: the host marks the extension failed and stops trying.
RESTART_BACKOFFS: tuple[float, float, float] = (1.0, 5.0, 25.0)

#: Idle interval (seconds) after which the host should health-probe with ``ping``.
PING_IDLE = 60.0

#: Bounded depth of the per-connection observe (``event``) lane. When a slow or
#: stalled extension lets events pile past this, the OLDEST are shed and an
#: ``events_lost`` marker is delivered on recovery — observe events are lossy by
#: contract, so shedding beats unbounded memory growth. Requests (hook/tool/ping/
#: shutdown) are NEVER shed; they ride the reliable control lane.
OBSERVE_QUEUE_CAP = 1024


def backoff_for(attempt: int) -> Optional[float]:
    """Backoff (seconds) for restart ``attempt`` (0-indexed). ``None`` once attempts
    are exhausted — the caller transitions the extension to failed."""
    if 0 <= attempt < len(RESTART_BACKOFFS):
        return RESTART_BACKOFFS[attempt]
    return None


class InboundHandler:
    """Handles ext->host requests and notifications. The default answers ``ping`` and
    rejects everything else with ``MethodNotFound``; the host supplies a richer
    implementation once ext->host methods (session/ui/kv/...) are wired."""

    async def handle_request(self, method_name: str, params: Any) -> Any:
        if method_name == method.PING:
            return {}
        raise RpcError(codes.METHOD_NOT_FOUND, f"method not found: {method_name}")

    def handle_notification(self, method_name: str, params: Any) -> None:
        pass


class DefaultInboundHandler(InboundHandler):
    """The trivial handler: ping only. Used when the host wires nothing richer."""


@dataclass
class SpawnSpec:
    """How to launch the subprocess. Deliberately small — the manifest owns the full
    shape; this is just what :meth:`ExtensionProcess.spawn` needs."""

    command: str
    args: list[str] = field(default_factory=list)
    env: dict[str, str] = field(default_factory=dict)
    #: Working directory for the child (the extension's root).
    cwd: Optional[Path] = None


class ObserveLane:
    """The per-connection observe lane: a bounded, oldest-shedding queue of ``event``
    frames plus a monotonic sequence and a shed counter. Fire-and-forget events go
    here so a stuck child stdin can't grow host memory without bound.

    Single-threaded under the asyncio loop, so the deque needs no lock."""

    def __init__(self) -> None:
        self._queue: deque[Message] = deque()
        self._seq = 0
        self._lost = 0
        self._last_context: Any = None

    def push(self, event: str, context: Any, payload: Any) -> None:
        """Enqueue an ``event`` frame, shedding the oldest if at capacity."""
        seq = self._seq
        self._seq += 1
        frame = Message.notification(method.EVENT, {"event": event, "seq": seq, "context": context, "payload": payload})
        if len(self._queue) >= OBSERVE_QUEUE_CAP:
            self._queue.popleft()
            self._lost += 1
        self._queue.append(frame)
        self._last_context = context

    def pop_for_write(self) -> Optional[Message]:
        """Next frame for the writer to flush, or ``None`` when drained. Emits an
        ``events_lost`` marker (no ``seq`` — it is out-of-band; a gap in the seq run
        signals the loss, the marker carries the exact count) before the surviving
        events whenever shedding happened since the last drain."""
        if self._lost > 0:
            lost = self._lost
            self._lost = 0
            return Message.notification(
                method.EVENT,
                {"event": "events_lost", "context": self._last_context, "payload": {"lost": lost}},
            )
        if self._queue:
            return self._queue.popleft()
        return None


class ExtensionProcess:
    """One extension subprocess."""

    def __init__(self, spec: SpawnSpec, handler: InboundHandler) -> None:
        self._spec = spec
        self._handler = handler
        self._pending: dict[int, asyncio.Future[Any]] = {}
        self._generation = 0
        self._next_id = 1
        self._alive = False
        # Live-connection state, replaced wholesale on respawn.
        self._proc: Optional[asyncio.subprocess.Process] = None
        self._control: deque[Message] = deque()
        self._observe = ObserveLane()
        self._wakeup: Optional[asyncio.Event] = None
        self._tasks: list[asyncio.Task[None]] = []

    @classmethod
    async def spawn(cls, spec: SpawnSpec, handler: InboundHandler) -> ExtensionProcess:
        """Spawn the subprocess and start its reader/writer tasks."""
        self = cls(spec, handler)
        await self._start_connection(0)
        self._alive = True
        return self

    async def _start_connection(self, my_generation: int) -> None:
        """Spawn the child and wire the reader/writer/stderr tasks for one
        generation. Shared by :meth:`spawn` and :meth:`respawn`."""
        env = {**os.environ, **self._spec.env}
        proc = await asyncio.create_subprocess_exec(
            self._spec.command,
            *self._spec.args,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            cwd=str(self._spec.cwd) if self._spec.cwd is not None else None,
            env=env,
        )
        self._proc = proc
        self._control = deque()
        self._observe = ObserveLane()
        self._wakeup = asyncio.Event()

        assert proc.stdout is not None and proc.stderr is not None
        self._tasks = [
            asyncio.ensure_future(self._writer_loop(proc, my_generation)),
            asyncio.ensure_future(self._reader_loop(proc.stdout, my_generation)),
            asyncio.ensure_future(self._stderr_loop(proc.stderr)),
        ]

    # -- writer ------------------------------------------------------------

    def _send_control(self, frame: Message) -> None:
        """Queue a reliable control frame (request/response/notification/cancel)."""
        self._control.append(frame)
        if self._wakeup is not None:
            self._wakeup.set()

    async def _writer_loop(self, proc: asyncio.subprocess.Process, my_generation: int) -> None:
        wakeup = self._wakeup
        assert wakeup is not None and proc.stdin is not None
        stdin = proc.stdin
        try:
            while True:
                await wakeup.wait()
                wakeup.clear()
                # Control frames (reliable) first, biased over the lossy observe lane.
                while self._control:
                    if not await _write_frame(stdin, self._control.popleft()):
                        return
                # Then drain the observe lane (events_lost marker first, if any).
                while True:
                    frame = self._observe.pop_for_write()
                    if frame is None:
                        break
                    if not await _write_frame(stdin, frame):
                        return
        except asyncio.CancelledError:
            raise

    # -- reader ------------------------------------------------------------

    async def _reader_loop(self, stdout: asyncio.StreamReader, my_generation: int) -> None:
        try:
            while True:
                line = await stdout.readline()
                if not line:  # EOF — the child is gone.
                    break
                text = line.decode("utf-8", errors="replace").strip()
                if not text:
                    continue
                await self._dispatch_line(text, my_generation)
        except asyncio.CancelledError:
            raise
        finally:
            # Only the current generation's reader may declare death and fail pending —
            # a stale reader must not disturb a fresh child.
            if self._generation == my_generation:
                self._alive = False
                self._fail_all_pending("extension connection closed")

    async def _dispatch_line(self, line: str, my_generation: int) -> None:
        try:
            data = json.loads(line)
        except json.JSONDecodeError as exc:
            _log.warning("extension: unparseable frame: %s (%s)", line, exc)
            return
        if not isinstance(data, dict):
            return
        msg = Message.from_dict(data)

        if msg.is_response():
            # Generation guard: drop responses that belong to a prior child.
            if self._generation != my_generation:
                return
            if not isinstance(msg.id, int):
                return
            fut = self._pending.pop(msg.id, None)
            if fut is None or fut.done():
                return
            if msg.error is not None:
                fut.set_exception(msg.error)
            else:
                fut.set_result(msg.result)
        elif msg.is_request():
            req_id = msg.id
            method_name = msg.method or ""
            params = msg.params
            try:
                result = await self._handler.handle_request(method_name, params)
                reply = Message.success(req_id, result)
            except RpcError as err:
                reply = Message.error_response(req_id, err)
            except Exception as exc:  # noqa: BLE001 — a handler bug is an internal error, not a crash
                reply = Message.error_response(req_id, RpcError(codes.INTERNAL_ERROR, str(exc)))
            self._send_control(reply)
        elif msg.is_notification():
            self._handler.handle_notification(msg.method or "", msg.params)

    async def _stderr_loop(self, stderr: asyncio.StreamReader) -> None:
        try:
            while True:
                line = await stderr.readline()
                if not line:
                    break
                _log.debug("ext stderr [%s]: %s", self._spec.command, line.decode("utf-8", "replace").rstrip())
        except asyncio.CancelledError:
            raise

    # -- public API --------------------------------------------------------

    async def request(self, method_name: str, params: Any, timeout: float) -> Any:
        """Send a request and await its response, bounded by ``timeout`` seconds.

        Raises :class:`RpcError` if the extension replies with an error, or
        :class:`RuntimeError` if the connection is dead or the request times out. On
        timeout or cancellation the peer is told to stop via ``$/cancel`` and the
        pending slot is cleared."""
        if not self._alive:
            raise RuntimeError("extension is not alive")
        req_id = self._next_id
        self._next_id += 1
        fut: asyncio.Future[Any] = asyncio.get_running_loop().create_future()
        self._pending[req_id] = fut
        self._send_control(Message.request(req_id, method_name, params))
        try:
            return await asyncio.wait_for(fut, timeout)
        except asyncio.TimeoutError as exc:
            self._pending.pop(req_id, None)
            self._cancel(req_id)
            raise RuntimeError(f"extension request `{method_name}` timed out after {timeout}s") from exc
        except asyncio.CancelledError:
            self._pending.pop(req_id, None)
            self._cancel(req_id)
            raise

    def notify(self, method_name: str, params: Any) -> None:
        """Send a fire-and-forget notification on the reliable control lane."""
        self._send_control(Message.notification(method_name, params))

    def _cancel(self, req_id: int) -> None:
        """Best-effort ``$/cancel`` for an in-flight request. A cancel for an
        already-answered id is a harmless no-op the peer ignores."""
        self.notify(method.CANCEL, {"id": req_id})

    def send_event(self, event: str, context: Any, payload: Any) -> None:
        """Enqueue an observe ``event`` on the bounded, lossy lane. Assigns a
        per-connection sequence number; sheds the oldest queued event (tracked for
        the next ``events_lost`` marker) rather than block or grow unbounded when the
        extension is not draining its stdin. Never fails — a shed event is the
        contract, not an error."""
        self._observe.push(event, context, payload)
        if self._wakeup is not None:
            self._wakeup.set()

    @property
    def is_alive(self) -> bool:
        return self._alive

    @property
    def generation(self) -> int:
        return self._generation

    async def ping_health(self, timeout: float) -> bool:
        """Health-probe with ``ping``. ``True`` if the extension answered in time."""
        try:
            await self.request(method.PING, {}, timeout)
            return True
        except (RpcError, RuntimeError):
            return False

    async def respawn(self) -> None:
        """Kill and re-spawn the child in place. Bumps the generation (invalidating
        any stale reader and failing every in-flight request), then starts a fresh
        connection. ``next_id`` is NOT reset, so ids never collide across
        generations."""
        self._generation += 1
        new_generation = self._generation
        self._fail_all_pending("extension restarting")
        await self._teardown_connection()
        await self._start_connection(new_generation)
        self._alive = True

    async def shutdown(self, grace: float) -> None:
        """Graceful shutdown: send ``shutdown``, wait up to ``grace`` for the reply,
        then force-kill. Always leaves the process dead."""
        try:
            await self.request(method.SHUTDOWN, {}, grace)
        except (RpcError, RuntimeError):
            pass
        self._alive = False
        await self._teardown_connection()

    async def _teardown_connection(self) -> None:
        """Abort the reader/writer/stderr tasks and kill the child."""
        for task in self._tasks:
            task.cancel()
        for task in self._tasks:
            try:
                await task
            except asyncio.CancelledError:
                pass
            except Exception:  # noqa: BLE001 — a dying task's error must not mask teardown
                pass
        self._tasks = []
        proc = self._proc
        if proc is not None and proc.returncode is None:
            try:
                proc.kill()
            except ProcessLookupError:
                pass
            try:
                await proc.wait()
            except Exception:  # noqa: BLE001
                pass

    def _fail_all_pending(self, reason: str) -> None:
        """Fail every pending request with the same error. Used on connection close
        and on respawn."""
        pending = self._pending
        self._pending = {}
        for fut in pending.values():
            if not fut.done():
                fut.set_exception(RpcError(codes.INTERNAL_ERROR, reason))


async def _write_frame(stdin: asyncio.StreamWriter, msg: Message) -> bool:
    """Serialize a frame as ndjson to the child stdin. Returns ``False`` on any write
    error (the caller tears the connection down)."""
    try:
        line = json.dumps(msg.to_dict(), separators=(",", ":")) + "\n"
    except (TypeError, ValueError) as exc:
        _log.warning("extension: failed to serialize outbound frame: %s", exc)
        return True  # a bad frame is not a broken pipe — keep the connection.
    try:
        stdin.write(line.encode("utf-8"))
        await stdin.drain()
        return True
    except (ConnectionResetError, BrokenPipeError, RuntimeError):
        return False
