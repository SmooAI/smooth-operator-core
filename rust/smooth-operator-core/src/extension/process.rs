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

use std::collections::HashMap;
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex as StdMutex};
use std::time::Duration;

use async_trait::async_trait;
use serde_json::Value;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, Command};
use tokio::sync::{mpsc, oneshot};

use super::protocol::{codes, method, Message, RpcError};

/// Backoff schedule for restart attempts. After the third failed attempt the
/// host marks the extension `Failed` and stops trying.
pub const RESTART_BACKOFFS: [Duration; 3] = [Duration::from_secs(1), Duration::from_secs(5), Duration::from_secs(25)];

/// Idle interval after which the host should health-probe with `ping`.
pub const PING_IDLE: Duration = Duration::from_secs(60);

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
#[derive(Debug, Clone)]
pub struct SpawnSpec {
    pub command: String,
    pub args: Vec<String>,
    pub env: HashMap<String, String>,
    /// Working directory for the child (the extension's root).
    pub cwd: Option<std::path::PathBuf>,
}

type PendingMap = Arc<StdMutex<HashMap<u64, oneshot::Sender<Result<Value, RpcError>>>>>;

/// A live child connection: the writer channel plus the abort handles for its
/// reader/writer/stderr tasks. Replaced wholesale on `respawn`.
struct Connection {
    outbound_tx: mpsc::UnboundedSender<Message>,
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
        let mut cmd = Command::new(&spec.command);
        cmd.args(&spec.args)
            .envs(&spec.env)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true);
        if let Some(cwd) = &spec.cwd {
            cmd.current_dir(cwd);
        }

        let mut child = cmd.spawn().map_err(|e| anyhow::anyhow!("spawn extension `{}`: {e}", spec.command))?;
        let stdin = child.stdin.take().ok_or_else(|| anyhow::anyhow!("no stdin pipe"))?;
        let stdout = child.stdout.take().ok_or_else(|| anyhow::anyhow!("no stdout pipe"))?;
        let stderr = child.stderr.take().ok_or_else(|| anyhow::anyhow!("no stderr pipe"))?;

        let (outbound_tx, mut outbound_rx) = mpsc::unbounded_channel::<Message>();

        // Writer task: drain outbound queue → child stdin as ndjson.
        let writer = tokio::spawn(async move {
            let mut stdin = stdin;
            while let Some(msg) = outbound_rx.recv().await {
                match serde_json::to_string(&msg) {
                    Ok(mut line) => {
                        line.push('\n');
                        if stdin.write_all(line.as_bytes()).await.is_err() || stdin.flush().await.is_err() {
                            break;
                        }
                    }
                    Err(e) => tracing::warn!(error = %e, "extension: failed to serialize outbound frame"),
                }
            }
        });

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

        match tokio::time::timeout(timeout, rx).await {
            Ok(Ok(Ok(value))) => Ok(value),
            Ok(Ok(Err(rpc))) => Err(anyhow::anyhow!(rpc)),
            Ok(Err(_recv)) => anyhow::bail!("extension dropped the request channel"),
            Err(_elapsed) => {
                self.pending.lock().expect("pending lock").remove(&id);
                anyhow::bail!("extension request `{method}` timed out after {timeout:?}");
            }
        }
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
