//! `ExtensionProcess` — one extension subprocess, its ndjson codec, and its
//! request/response plumbing.
//!
//! Framing is identical to MCP stdio: one JSON-RPC message per line on the
//! child's stdin/stdout, stderr drained to host tracing. A reader task routes
//! inbound responses to their pending caller and inbound requests to an
//! [`InboundHandler`]; a writer task serializes outbound frames.
//!
//! Restart is in-place ([`ExtensionProcess::respawn`]): a generation counter is
//! bumped so a stale reader from the dead child can't resolve a request
//! registered against the new child, and every in-flight request fails fast.

use std::collections::{HashMap, VecDeque};
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex as StdMutex};
use std::time::Duration;

use async_trait::async_trait;
use serde_json::Value;
use sha2::{Digest, Sha256};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, Command};
use tokio::sync::{mpsc, oneshot, Notify};

use super::protocol::{codes, method, Message, RpcError};

/// Backoff schedule for restart attempts. After the third failed attempt the
/// host marks the extension `Failed` and stops trying.
pub const RESTART_BACKOFFS: [Duration; 3] = [Duration::from_secs(1), Duration::from_secs(5), Duration::from_secs(25)];

/// Idle interval after which the host should health-probe with `ping`.
pub const PING_IDLE: Duration = Duration::from_secs(60);

/// Bounded depth of the per-connection observe (`event`) lane. When a slow or
/// stalled extension lets events pile past this, the OLDEST are shed and an
/// `events_lost` marker is delivered on recovery — observe events are lossy by
/// contract, so shedding beats unbounded memory growth. Requests (hook/tool/
/// ping/shutdown) are NEVER shed; they ride the reliable control lane.
pub const OBSERVE_QUEUE_CAP: usize = 1024;

/// The per-connection observe lane: a bounded, oldest-shedding queue of `event`
/// frames plus a monotonic sequence and a shed counter. Fire-and-forget events
/// go here so a stuck child stdin can't grow host memory without bound.
#[derive(Debug, Default)]
struct ObserveLane {
    queue: StdMutex<VecDeque<Message>>,
    /// Per-connection monotonic event sequence (labels events so a subscriber
    /// can detect a gap from shedding).
    seq: AtomicU64,
    /// Events shed since the last `events_lost` marker was drained.
    lost: AtomicU64,
    /// The dispatch context of the most recent event, reused on the out-of-band
    /// `events_lost` marker so every delivered event carries a `context`.
    last_context: StdMutex<Value>,
    /// Signals the writer task that the queue has work.
    notify: Notify,
}

impl ObserveLane {
    /// Enqueue an `event` frame, shedding the oldest if at capacity.
    fn push(&self, event: &str, context: &Value, payload: Value) {
        let seq = self.seq.fetch_add(1, Ordering::SeqCst);
        let frame = Message::notification(
            method::EVENT,
            serde_json::json!({ "event": event, "seq": seq, "context": context, "payload": payload }),
        );
        {
            let mut q = self.queue.lock().expect("observe queue lock");
            if q.len() >= OBSERVE_QUEUE_CAP {
                q.pop_front();
                self.lost.fetch_add(1, Ordering::SeqCst);
            }
            q.push_back(frame);
        }
        *self.last_context.lock().expect("observe ctx lock") = context.clone();
        self.notify.notify_one();
    }

    /// Next frame for the writer to flush, or `None` when drained. Emits an
    /// `events_lost` marker (no `seq` — it is out-of-band, a gap in the seq run
    /// signals the loss; the marker carries the exact count) before the
    /// surviving events whenever shedding happened since the last drain.
    fn pop_for_write(&self) -> Option<Message> {
        let lost = self.lost.swap(0, Ordering::SeqCst);
        if lost > 0 {
            let context = self.last_context.lock().expect("observe ctx lock").clone();
            return Some(Message::notification(
                method::EVENT,
                serde_json::json!({ "event": "events_lost", "context": context, "payload": { "lost": lost } }),
            ));
        }
        self.queue.lock().expect("observe queue lock").pop_front()
    }
}

/// Backoff for restart `attempt` (0-indexed). `None` once attempts are
/// exhausted — the caller transitions the extension to `Failed`.
#[must_use]
pub fn backoff_for(attempt: usize) -> Option<Duration> {
    RESTART_BACKOFFS.get(attempt).copied()
}

/// Handles ext→host requests and notifications. The default answers `ping` and
/// rejects everything else with `MethodNotFound`; the host supplies a richer
/// implementation once ext→host methods (session/ui/kv/…) are wired.
#[async_trait]
pub trait InboundHandler: Send + Sync {
    async fn handle_request(&self, method: &str, params: Value) -> Result<Value, RpcError> {
        let _ = params;
        if method == method::PING {
            Ok(serde_json::json!({}))
        } else {
            Err(RpcError::new(codes::METHOD_NOT_FOUND, format!("method not found: {method}")))
        }
    }

    fn handle_notification(&self, method: &str, params: Value) {
        let _ = (method, params);
    }
}

/// The trivial handler: ping only. Used when the host wires nothing richer.
#[derive(Debug, Default)]
pub struct DefaultInboundHandler;

impl InboundHandler for DefaultInboundHandler {}

/// How to launch the subprocess. Deliberately small — the manifest owns the
/// full shape; this is just what `spawn` needs.
#[derive(Debug, Clone, Default)]
pub struct SpawnSpec {
    pub command: String,
    pub args: Vec<String>,
    /// Extra env vars the extension legitimately needs (its manifest `[run] env`,
    /// SEP-protocol vars). These are the ONLY env the child sees beyond the small
    /// [`ENV_PASSTHROUGH`] allow-list — the host's full environment is scrubbed
    /// (`.env_clear()`) so ambient secrets can't leak in (the lethal-trifecta
    /// concern). See [`build_child_env`].
    pub env: HashMap<String, String>,
    /// Working directory for the child (the extension's root).
    pub cwd: Option<std::path::PathBuf>,
    /// Optional pinned SHA-256 (lowercase hex) of the resolved `command` binary.
    /// `Some` → the child is refused to spawn unless the on-disk binary hashes to
    /// exactly this. `None` → TOFU: the observed hash is logged so it can be
    /// pinned. See [`verify_integrity`].
    pub sha256: Option<String>,
}

/// The ONLY host environment variables passed through to an extension
/// subprocess. Everything else is scrubbed (`.env_clear()` in
/// [`start_connection`]) so ambient secrets — cloud creds (`AWS_SECRET_ACCESS_KEY`),
/// API tokens, `GITHUB_TOKEN`, … — can never leak into an extension via inherited
/// env (the lethal-trifecta concern from the guardrail work).
///
/// These are launch essentials only, and none is secret:
/// - `PATH` — resolve a bare-name interpreter (`node`, `python3`) and its own
///   subprocess lookups. Without it a non-absolute `command` won't even start.
/// - `HOME` — interpreters (node, python) read user config/caches from it and
///   some abort without it.
/// - `LANG` / `LC_ALL` / `LC_CTYPE` — locale; python3 in particular errors on
///   non-ASCII I/O when unset.
/// - `TMPDIR` — where the child writes temp files (macOS/BSD).
/// - `TERM` — interpreters that probe for a tty degrade gracefully with it.
/// - `SystemRoot` — Windows: `node.exe`/`python.exe` fail to start without it.
///
/// SEP-protocol vars and anything else an extension legitimately needs come
/// through its manifest `[run] env` (carried in [`SpawnSpec::env`]), NOT from
/// here — so adding a var is a deliberate, per-extension act, never ambient.
pub const ENV_PASSTHROUGH: &[&str] = &["PATH", "HOME", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR", "TERM", "SystemRoot"];

/// Build the exact environment an extension child sees: the [`ENV_PASSTHROUGH`]
/// allow-list pulled from the host (via `lookup`), then `explicit` (the manifest
/// env) overlaid on top so an extension can still *set* — but never silently
/// *inherit* — any var. `lookup` is injected so this is a pure, exhaustively
/// testable function (the caller passes `|k| std::env::var(k).ok()`).
fn build_child_env<F: Fn(&str) -> Option<String>>(lookup: F, explicit: &HashMap<String, String>) -> HashMap<String, String> {
    let mut env: HashMap<String, String> = ENV_PASSTHROUGH.iter().filter_map(|k| lookup(k).map(|v| ((*k).to_string(), v))).collect();
    // Manifest env wins on collision (an extension may legitimately override PATH).
    env.extend(explicit.iter().map(|(k, v)| (k.clone(), v.clone())));
    env
}

/// Lowercase-hex encode `bytes` (no `hex` crate — one fold).
fn to_hex(bytes: &[u8]) -> String {
    use std::fmt::Write;
    bytes.iter().fold(String::with_capacity(bytes.len() * 2), |mut s, b| {
        let _ = write!(s, "{b:02x}");
        s
    })
}

/// Resolve `command` to the file that will actually execute: an absolute or
/// relative path is used verbatim (if it exists); a bare name is searched on
/// `PATH` (first existing file wins — the same rule the OS applies). `None` when
/// nothing matches (a bare name not on PATH would fail at spawn anyway).
fn resolve_command_path(command: &str) -> Option<PathBuf> {
    let p = Path::new(command);
    if p.is_absolute() || p.components().count() > 1 {
        return p.exists().then(|| p.to_path_buf());
    }
    let path = std::env::var_os("PATH")?;
    std::env::split_paths(&path).map(|dir| dir.join(command)).find(|c| c.is_file())
}

/// SHA-256 (lowercase hex) of a file's bytes.
fn file_sha256(path: &Path) -> std::io::Result<String> {
    let mut hasher = Sha256::new();
    hasher.update(std::fs::read(path)?);
    Ok(to_hex(&hasher.finalize()))
}

/// Compare an `observed` hash against an optional pinned `expected` (integrity
/// verification's decision core — kept pure so it can be tested exhaustively).
///
/// - `expected` is `None` → `Ok` (TOFU: the caller logs `observed` so a consumer
///   can pin it).
/// - `expected` matches (case-insensitive hex) → `Ok`.
/// - `expected` mismatches → `Err` with a clear, non-leaky message.
fn verify_integrity(observed: &str, expected: Option<&str>) -> Result<(), String> {
    match expected {
        None => Ok(()),
        Some(want) if want.eq_ignore_ascii_case(observed) => Ok(()),
        Some(want) => Err(format!(
            "extension integrity check FAILED: expected sha256 {}, but the binary on disk hashes to {observed} — refusing to spawn",
            want.to_ascii_lowercase()
        )),
    }
}

/// Enforce the integrity gate for `spec` before spawning: hash the resolved
/// command binary and compare to the pin. A pinned-but-mismatching binary is
/// refused; an unpinned one is allowed and its hash logged (TOFU). An
/// unresolvable command with a pin is refused (can't verify what we can't find);
/// without a pin it's left to fail at spawn with the OS's own error.
fn enforce_integrity(spec: &SpawnSpec) -> anyhow::Result<()> {
    let Some(path) = resolve_command_path(&spec.command) else {
        if spec.sha256.is_some() {
            anyhow::bail!(
                "extension `{}`: integrity pin set but the command binary could not be resolved to verify",
                spec.command
            );
        }
        return Ok(());
    };
    let observed = file_sha256(&path).map_err(|e| anyhow::anyhow!("hashing `{}` for integrity check: {e}", path.display()))?;
    verify_integrity(&observed, spec.sha256.as_deref()).map_err(|e| anyhow::anyhow!(e))?;
    if spec.sha256.is_none() {
        tracing::info!(command = %spec.command, path = %path.display(), sha256 = %observed, "extension: no integrity pin — record this sha256 in the manifest `[run] sha256` to pin it (TOFU)");
    } else {
        tracing::debug!(command = %spec.command, sha256 = %observed, "extension: integrity verified against pinned sha256");
    }
    Ok(())
}

type PendingMap = Arc<StdMutex<HashMap<u64, oneshot::Sender<Result<Value, RpcError>>>>>;

/// A live child connection: the writer channel plus the abort handles for its
/// reader/writer/stderr tasks. Replaced wholesale on `respawn`.
struct Connection {
    outbound_tx: mpsc::UnboundedSender<Message>,
    /// The bounded, lossy lane for fire-and-forget `event` frames.
    observe: Arc<ObserveLane>,
    child: Child,
    tasks: Vec<tokio::task::JoinHandle<()>>,
}

impl Connection {
    fn abort(&mut self) {
        for t in &self.tasks {
            t.abort();
        }
        // Best-effort kill; the child may already be gone.
        let _ = self.child.start_kill();
    }
}

/// One extension subprocess.
pub struct ExtensionProcess {
    spec: SpawnSpec,
    handler: Arc<dyn InboundHandler>,
    pending: PendingMap,
    generation: Arc<AtomicU64>,
    next_id: AtomicU64,
    alive: Arc<AtomicBool>,
    conn: StdMutex<Connection>,
}

impl std::fmt::Debug for ExtensionProcess {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ExtensionProcess")
            .field("command", &self.spec.command)
            .field("generation", &self.generation.load(Ordering::SeqCst))
            .field("alive", &self.alive.load(Ordering::SeqCst))
            .finish()
    }
}

impl ExtensionProcess {
    /// Spawn the subprocess and start its reader/writer tasks.
    ///
    /// # Errors
    /// Returns an error if the process cannot be spawned or its stdio can't be
    /// captured.
    pub fn spawn(spec: SpawnSpec, handler: Arc<dyn InboundHandler>) -> anyhow::Result<Self> {
        let pending: PendingMap = Arc::new(StdMutex::new(HashMap::new()));
        let generation = Arc::new(AtomicU64::new(0));
        let alive = Arc::new(AtomicBool::new(true));
        let conn = Self::start_connection(&spec, &handler, &pending, &generation, &alive, 0)?;
        Ok(Self {
            spec,
            handler,
            pending,
            generation,
            next_id: AtomicU64::new(1),
            alive,
            conn: StdMutex::new(conn),
        })
    }

    /// Spawn the child and wire the reader/writer/stderr tasks for one
    /// generation. Shared by `spawn` and `respawn`.
    fn start_connection(
        spec: &SpawnSpec,
        handler: &Arc<dyn InboundHandler>,
        pending: &PendingMap,
        generation: &Arc<AtomicU64>,
        alive: &Arc<AtomicBool>,
        my_generation: u64,
    ) -> anyhow::Result<Connection> {
        // Integrity gate (th-210910): even an allow-listed extension binary must
        // match its recorded hash before we spawn it. Refuses on mismatch.
        enforce_integrity(spec)?;

        let mut cmd = Command::new(&spec.command);
        cmd.args(&spec.args)
            // Scrub the host environment: the child starts from nothing and sees
            // only the ENV_PASSTHROUGH launch essentials + the manifest's explicit
            // env. Ambient secrets (cloud creds, tokens) never inherit through.
            .env_clear()
            .envs(build_child_env(|k| std::env::var(k).ok(), &spec.env))
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            // Only the three stdio pipes are handed to the child; Rust sets
            // CLOEXEC on every other host fd, so nothing extra leaks in.
            .kill_on_drop(true);
        if let Some(cwd) = &spec.cwd {
            cmd.current_dir(cwd);
        }

        let mut child = cmd.spawn().map_err(|e| anyhow::anyhow!("spawn extension `{}`: {e}", spec.command))?;
        let stdin = child.stdin.take().ok_or_else(|| anyhow::anyhow!("no stdin pipe"))?;
        let stdout = child.stdout.take().ok_or_else(|| anyhow::anyhow!("no stdout pipe"))?;
        let stderr = child.stderr.take().ok_or_else(|| anyhow::anyhow!("no stderr pipe"))?;

        let (outbound_tx, mut outbound_rx) = mpsc::unbounded_channel::<Message>();
        let observe = Arc::new(ObserveLane::default());

        // Writer task: control frames (requests/responses/cancel) ride the
        // reliable unbounded lane and always win; the bounded observe lane is
        // drained whenever it signals or after each control write. A stalled
        // child stdin blocks the write, but the observe lane sheds oldest under
        // its own lock meanwhile, so host memory stays bounded regardless.
        let writer = {
            let observe = Arc::clone(&observe);
            tokio::spawn(async move {
                let mut stdin = stdin;
                loop {
                    let ctrl = tokio::select! {
                        biased;
                        frame = outbound_rx.recv() => match frame {
                            Some(m) => Some(m),
                            None => break, // control lane closed → connection torn down
                        },
                        () = observe.notify.notified() => None,
                    };
                    if let Some(msg) = ctrl {
                        if !write_frame(&mut stdin, &msg).await {
                            break;
                        }
                    }
                    // Flush the observe lane (events_lost marker first, if any).
                    while let Some(msg) = observe.pop_for_write() {
                        if !write_frame(&mut stdin, &msg).await {
                            return;
                        }
                    }
                }
            })
        };

        // stderr drain → tracing.
        let cmd_name = spec.command.clone();
        let stderr_task = tokio::spawn(async move {
            let mut lines = BufReader::new(stderr).lines();
            while let Ok(Some(line)) = lines.next_line().await {
                tracing::debug!(extension = %cmd_name, "ext stderr: {line}");
            }
        });

        // Reader task: child stdout → route responses/requests.
        let reader = {
            let pending = Arc::clone(pending);
            let generation = Arc::clone(generation);
            let alive = Arc::clone(alive);
            let handler = Arc::clone(handler);
            let reply_tx = outbound_tx.clone();
            tokio::spawn(async move {
                let mut lines = BufReader::new(stdout).lines();
                // Loop ends on EOF or read error — either way the child is gone.
                while let Ok(Some(line)) = lines.next_line().await {
                    if line.trim().is_empty() {
                        continue;
                    }
                    Self::dispatch_line(&line, my_generation, &generation, &pending, &handler, &reply_tx).await;
                }
                // Only the current generation's reader may declare death and
                // fail pending — a stale reader must not disturb a fresh child.
                if generation.load(Ordering::SeqCst) == my_generation {
                    alive.store(false, Ordering::SeqCst);
                    fail_all_pending(&pending, "extension connection closed");
                }
            })
        };

        Ok(Connection {
            outbound_tx,
            observe,
            child,
            tasks: vec![writer, reader, stderr_task],
        })
    }

    /// Parse and route one inbound line.
    async fn dispatch_line(
        line: &str,
        my_generation: u64,
        generation: &Arc<AtomicU64>,
        pending: &PendingMap,
        handler: &Arc<dyn InboundHandler>,
        reply_tx: &mpsc::UnboundedSender<Message>,
    ) {
        let msg: Message = match serde_json::from_str(line) {
            Ok(m) => m,
            Err(e) => {
                tracing::warn!(error = %e, line, "extension: unparseable frame");
                return;
            }
        };

        if msg.is_response() {
            // Generation guard: drop responses that belong to a prior child.
            if generation.load(Ordering::SeqCst) != my_generation {
                return;
            }
            let Some(id) = msg.id.as_ref().and_then(Value::as_u64) else {
                return;
            };
            let sender = pending.lock().expect("pending lock").remove(&id);
            if let Some(tx) = sender {
                let payload = match msg.error {
                    Some(err) => Err(err),
                    None => Ok(msg.result.unwrap_or(Value::Null)),
                };
                let _ = tx.send(payload);
            }
        } else if msg.is_request() {
            let (id, method_name) = (msg.id.clone().unwrap_or(Value::Null), msg.method.clone().unwrap_or_default());
            let params = msg.params.unwrap_or(Value::Null);
            let reply = match handler.handle_request(&method_name, params).await {
                Ok(result) => Message::success(id, result),
                Err(err) => Message::error_response(Some(id), err),
            };
            let _ = reply_tx.send(reply);
        } else if msg.is_notification() {
            handler.handle_notification(&msg.method.unwrap_or_default(), msg.params.unwrap_or(Value::Null));
        }
    }

    /// Send a request and await its response, bounded by `timeout`.
    ///
    /// # Errors
    /// Returns an error if the connection is dead, the send fails, the request
    /// times out, or the extension replies with a JSON-RPC error.
    pub async fn request(&self, method: &str, params: Value, timeout: Duration) -> anyhow::Result<Value> {
        if !self.alive.load(Ordering::SeqCst) {
            anyhow::bail!("extension is not alive");
        }
        let id = self.next_id.fetch_add(1, Ordering::SeqCst);
        let (tx, rx) = oneshot::channel();
        self.pending.lock().expect("pending lock").insert(id, tx);

        let frame = Message::request(Value::from(id), method, params);
        {
            let conn = self.conn.lock().expect("conn lock");
            if conn.outbound_tx.send(frame).is_err() {
                self.pending.lock().expect("pending lock").remove(&id);
                anyhow::bail!("extension writer is gone");
            }
        }

        // If this future is dropped (task cancelled) or times out before the
        // peer answers, tell the peer to stop working on the request via
        // `$/cancel` and clear the pending slot. Disarmed once the peer replies.
        let mut guard = CancelGuard { proc: self, id, armed: true };

        match tokio::time::timeout(timeout, rx).await {
            Ok(Ok(Ok(value))) => {
                guard.armed = false;
                Ok(value)
            }
            Ok(Ok(Err(rpc))) => {
                guard.armed = false;
                Err(anyhow::anyhow!(rpc))
            }
            Ok(Err(_recv)) => {
                guard.armed = false;
                anyhow::bail!("extension dropped the request channel")
            }
            // Leave the guard armed: its Drop clears pending and sends `$/cancel`.
            Err(_elapsed) => anyhow::bail!("extension request `{method}` timed out after {timeout:?}"),
        }
    }

    /// Best-effort `$/cancel` for an in-flight request `id`. The peer SHOULD
    /// stop work and reply to the original request with -32800 Cancelled; a
    /// cancel for an already-answered id is a harmless no-op the peer ignores.
    ///
    /// # Errors
    /// Returns an error only if the writer channel is closed.
    pub fn cancel(&self, id: u64) -> anyhow::Result<()> {
        self.notify(method::CANCEL, serde_json::json!({ "id": id }))
    }

    /// Send a fire-and-forget notification.
    ///
    /// # Errors
    /// Returns an error only if the writer channel is closed.
    pub fn notify(&self, method: &str, params: Value) -> anyhow::Result<()> {
        let conn = self.conn.lock().expect("conn lock");
        conn.outbound_tx
            .send(Message::notification(method, params))
            .map_err(|_| anyhow::anyhow!("extension writer is gone"))
    }

    /// Enqueue an observe `event` on the bounded, lossy lane. Assigns the frame
    /// a per-connection sequence number; sheds the oldest queued event (tracked
    /// for the next `events_lost` marker) rather than block or grow unbounded
    /// when the extension is not draining its stdin. Never fails — a shed event
    /// is the contract, not an error.
    pub fn send_event(&self, event: &str, context: &Value, payload: Value) {
        // Clone the Arc out from under the conn lock so the (brief) queue lock
        // isn't nested inside it.
        let observe = {
            let conn = self.conn.lock().expect("conn lock");
            Arc::clone(&conn.observe)
        };
        observe.push(event, context, payload);
    }

    /// Whether the connection is currently believed alive.
    #[must_use]
    pub fn is_alive(&self) -> bool {
        self.alive.load(Ordering::SeqCst)
    }

    /// Current generation (increments on every successful respawn).
    #[must_use]
    pub fn generation(&self) -> u64 {
        self.generation.load(Ordering::SeqCst)
    }

    /// Health-probe with `ping`. Returns `true` if the extension answered
    /// within `timeout`.
    pub async fn ping_health(&self, timeout: Duration) -> bool {
        self.request(method::PING, serde_json::json!({}), timeout).await.is_ok()
    }

    /// Kill and re-spawn the child in place. Bumps the generation (invalidating
    /// any stale reader and failing every in-flight request), then starts a
    /// fresh connection. `next_id` is NOT reset, so ids never collide across
    /// generations.
    ///
    /// # Errors
    /// Returns an error if the new child cannot be spawned; the old connection
    /// is torn down regardless.
    pub fn respawn(&self) -> anyhow::Result<()> {
        let new_generation = self.generation.fetch_add(1, Ordering::SeqCst) + 1;
        fail_all_pending(&self.pending, "extension restarting");

        {
            let mut conn = self.conn.lock().expect("conn lock");
            conn.abort();
        }

        let new_conn = Self::start_connection(&self.spec, &self.handler, &self.pending, &self.generation, &self.alive, new_generation)?;
        self.alive.store(true, Ordering::SeqCst);
        *self.conn.lock().expect("conn lock") = new_conn;
        Ok(())
    }

    /// Graceful shutdown: send `shutdown`, wait up to `grace` for the reply,
    /// then force-kill. Always leaves the process dead.
    pub async fn shutdown(&self, grace: Duration) {
        let _ = self.request(method::SHUTDOWN, serde_json::json!({}), grace).await;
        self.alive.store(false, Ordering::SeqCst);
        let mut conn = self.conn.lock().expect("conn lock");
        conn.abort();
    }
}

impl Drop for ExtensionProcess {
    fn drop(&mut self) {
        if let Ok(mut conn) = self.conn.lock() {
            conn.abort();
        }
    }
}

/// Sends `$/cancel` for an in-flight request if the awaiting future is dropped
/// or times out before the peer answers. Disarmed on a peer reply. Also clears
/// the pending slot so a late reply resolves to nothing instead of leaking.
struct CancelGuard<'a> {
    proc: &'a ExtensionProcess,
    id: u64,
    armed: bool,
}

impl Drop for CancelGuard<'_> {
    fn drop(&mut self) {
        if !self.armed {
            return;
        }
        self.proc.pending.lock().expect("pending lock").remove(&self.id);
        let _ = self.proc.cancel(self.id);
    }
}

/// Serialize a frame as ndjson to the child stdin. Returns `false` on any write
/// error (the caller tears the connection down).
async fn write_frame(stdin: &mut tokio::process::ChildStdin, msg: &Message) -> bool {
    match serde_json::to_string(msg) {
        Ok(mut line) => {
            line.push('\n');
            stdin.write_all(line.as_bytes()).await.is_ok() && stdin.flush().await.is_ok()
        }
        Err(e) => {
            tracing::warn!(error = %e, "extension: failed to serialize outbound frame");
            true // a bad frame is not a broken pipe — keep the connection.
        }
    }
}

/// Fail every pending request with the same error message. Used on connection
/// close and on respawn.
fn fail_all_pending(pending: &PendingMap, reason: &str) {
    let drained: Vec<_> = pending.lock().expect("pending lock").drain().map(|(_, tx)| tx).collect();
    for tx in drained {
        let _ = tx.send(Err(RpcError::new(codes::INTERNAL_ERROR, reason)));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn backoff_schedule() {
        assert_eq!(backoff_for(0), Some(Duration::from_secs(1)));
        assert_eq!(backoff_for(1), Some(Duration::from_secs(5)));
        assert_eq!(backoff_for(2), Some(Duration::from_secs(25)));
        assert_eq!(backoff_for(3), None);
    }

    #[test]
    fn backoff_exhausts_after_three() {
        backoff_schedule();
    }

    // ---- subprocess hardening (th-210910), exhaustively ----

    #[test]
    fn build_child_env_scrubs_ambient_secrets_keeps_allowlist_and_explicit() {
        // A fake host env: two allow-listed vars + a secret that must NOT pass.
        let host: HashMap<&str, &str> = HashMap::from([
            ("PATH", "/usr/bin:/bin"),
            ("HOME", "/home/tester"),
            ("AWS_SECRET_ACCESS_KEY", "super-secret"),
            ("GITHUB_TOKEN", "ghp_leak"),
            ("SOME_RANDOM_VAR", "x"),
        ]);
        let lookup = |k: &str| host.get(k).map(|v| (*v).to_string());
        let explicit = HashMap::from([("SEP_PROTO".to_string(), "1".to_string())]);

        let env = build_child_env(lookup, &explicit);

        // Allow-listed launch essentials pass through.
        assert_eq!(env.get("PATH").map(String::as_str), Some("/usr/bin:/bin"));
        assert_eq!(env.get("HOME").map(String::as_str), Some("/home/tester"));
        // The manifest's explicit (SEP-protocol) var passes through.
        assert_eq!(env.get("SEP_PROTO").map(String::as_str), Some("1"));
        // The lethal-trifecta concern: ambient secrets are SCRUBBED.
        assert!(!env.contains_key("AWS_SECRET_ACCESS_KEY"), "secret must not inherit");
        assert!(!env.contains_key("GITHUB_TOKEN"), "token must not inherit");
        assert!(!env.contains_key("SOME_RANDOM_VAR"), "non-allowlisted host var must not inherit");
    }

    #[test]
    fn build_child_env_explicit_overrides_passthrough() {
        let host: HashMap<&str, &str> = HashMap::from([("PATH", "/host/path")]);
        let explicit = HashMap::from([("PATH".to_string(), "/ext/path".to_string())]);
        let env = build_child_env(|k: &str| host.get(k).map(|v| (*v).to_string()), &explicit);
        assert_eq!(env.get("PATH").map(String::as_str), Some("/ext/path"), "manifest env wins on collision");
    }

    #[test]
    fn build_child_env_missing_passthrough_absent() {
        // Host has none of the allow-list set → child env is just the explicit map.
        let env = build_child_env(|_: &str| None, &HashMap::from([("A".to_string(), "b".to_string())]));
        assert_eq!(env.len(), 1);
        assert_eq!(env.get("A").map(String::as_str), Some("b"));
    }

    #[test]
    fn verify_integrity_none_is_tofu_ok() {
        assert!(verify_integrity("deadbeef", None).is_ok());
    }

    #[test]
    fn verify_integrity_match_allows_case_insensitive() {
        assert!(verify_integrity("abcDEF123", Some("ABCdef123")).is_ok());
    }

    #[test]
    fn verify_integrity_mismatch_refuses() {
        let err = verify_integrity("aaaa", Some("bbbb")).unwrap_err();
        assert!(err.contains("integrity check FAILED"), "{err}");
        assert!(err.contains("refusing to spawn"), "{err}");
    }

    #[test]
    fn to_hex_encodes_lowercase() {
        assert_eq!(to_hex(&[0x00, 0x0f, 0xff, 0xa5]), "000fffa5");
        assert_eq!(to_hex(&[]), "");
    }

    #[test]
    fn file_sha256_matches_known_vector() {
        // sha256("abc") — the canonical NIST test vector.
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("f");
        std::fs::write(&path, b"abc").unwrap();
        assert_eq!(file_sha256(&path).unwrap(), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
    }

    #[test]
    fn resolve_command_path_absolute_existing_and_missing() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("bin");
        std::fs::write(&path, b"x").unwrap();
        assert_eq!(resolve_command_path(path.to_str().unwrap()), Some(path.clone()));
        assert_eq!(resolve_command_path(dir.path().join("nope").to_str().unwrap()), None);
    }

    #[test]
    fn enforce_integrity_refuses_wrong_pin_allows_right_pin() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("ext-bin");
        std::fs::write(&path, b"abc").unwrap();
        let real = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad";

        let good = SpawnSpec {
            command: path.to_string_lossy().into_owned(),
            sha256: Some(real.to_string()),
            ..Default::default()
        };
        assert!(enforce_integrity(&good).is_ok(), "correct pin spawns");

        let bad = SpawnSpec {
            command: path.to_string_lossy().into_owned(),
            sha256: Some("00".repeat(32)),
            ..Default::default()
        };
        assert!(enforce_integrity(&bad).is_err(), "wrong pin refuses");

        // No pin → TOFU, allowed.
        let tofu = SpawnSpec {
            command: path.to_string_lossy().into_owned(),
            sha256: None,
            ..Default::default()
        };
        assert!(enforce_integrity(&tofu).is_ok(), "unpinned is allowed (TOFU)");
    }

    #[test]
    fn enforce_integrity_pin_but_unresolvable_command_refuses() {
        let spec = SpawnSpec {
            command: "/no/such/binary/xyz".to_string(),
            sha256: Some("00".repeat(32)),
            ..Default::default()
        };
        assert!(enforce_integrity(&spec).is_err(), "can't verify an unresolvable pinned command → refuse");
    }

    #[test]
    fn observe_lane_sheds_oldest_and_marks_loss() {
        let lane = ObserveLane::default();
        let ctx = serde_json::json!({ "token": "e", "tier": "event" });
        // Overflow by 3: push CAP+3 events.
        for i in 0..(OBSERVE_QUEUE_CAP + 3) {
            lane.push("turn_start", &ctx, serde_json::json!({ "n": i }));
        }
        assert_eq!(lane.queue.lock().unwrap().len(), OBSERVE_QUEUE_CAP);
        assert_eq!(lane.lost.load(Ordering::SeqCst), 3);
        // seq advanced once per push, regardless of shedding.
        assert_eq!(lane.seq.load(Ordering::SeqCst), (OBSERVE_QUEUE_CAP + 3) as u64);

        // First drain frame is the events_lost marker carrying the shed count.
        let marker = lane.pop_for_write().expect("marker");
        let p = marker.params.unwrap();
        assert_eq!(p["event"], "events_lost");
        assert_eq!(p["payload"]["lost"], 3);
        assert!(p.get("seq").is_none(), "marker is out-of-band, no seq");
        assert!(p.get("context").is_some(), "marker carries context so it satisfies the event schema");
        assert_eq!(lane.lost.load(Ordering::SeqCst), 0, "loss counter reset once marked");

        // The surviving events are the NEWEST (oldest shed): first is n=3.
        let first = lane.pop_for_write().expect("event");
        assert_eq!(first.params.unwrap()["payload"]["n"], 3);
    }

    #[test]
    fn observe_lane_no_marker_when_no_loss() {
        let lane = ObserveLane::default();
        let ctx = serde_json::json!({ "token": "e", "tier": "event" });
        lane.push("turn_start", &ctx, serde_json::json!({}));
        let f = lane.pop_for_write().expect("event");
        assert_eq!(f.params.unwrap()["event"], "turn_start");
        assert!(lane.pop_for_write().is_none());
    }

    #[tokio::test]
    async fn default_handler_answers_ping_only() {
        let h = DefaultInboundHandler;
        assert!(h.handle_request(method::PING, Value::Null).await.is_ok());
        let err = h.handle_request("session/send_message", Value::Null).await.unwrap_err();
        assert_eq!(err.code, codes::METHOD_NOT_FOUND);
    }

    // Live subprocess lifecycle (spawn / handshake / timeout / restart /
    // generation-guard) is exercised in `tests/sep_process.rs`, an integration
    // test where cargo defines `CARGO_BIN_EXE_sep-echo-peer` — that env is not
    // set for lib unit tests.
}
