//! `ExtensionHost` — orchestrates the loaded extensions: hook chaining in load
//! order, non-blocking event fanout, tool proxies, and the ext→host delegate
//! seam.
//!
//! The security-critical part is [`fold_hook_chain`]: how per-extension hook
//! outcomes combine, and what happens on timeout/crash. It is a pure function
//! so it can be tested exhaustively against adversarial inputs without spawning
//! anything.

use std::collections::HashSet;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use serde_json::{json, Value};

use super::manifest::{DiscoveredExtension, Scope};
use super::process::{ExtensionProcess, InboundHandler, SpawnSpec};
use super::protocol::{codes, method, Context, HookOutcome, HostInfo, InitializeParams, InitializeResult, RpcError, Tier, WorkspaceInfo};
use super::tool_proxy::ExtensionTool;
use crate::tool::Tool;

/// The SEP protocol version this host implements.
pub const PROTOCOL_VERSION: u32 = 1;

/// Classifies a hook by its failure policy and default timeout.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum HookType {
    ToolCall,
    UserBash,
    ToolResult,
    Input,
    BeforeAgentStart,
    Context,
    BeforeProviderRequest,
    MessageEnd,
    SessionBeforeCompact,
    SessionBeforeTree,
}

impl HookType {
    #[must_use]
    pub fn as_str(self) -> &'static str {
        match self {
            HookType::ToolCall => "tool_call",
            HookType::UserBash => "user_bash",
            HookType::ToolResult => "tool_result",
            HookType::Input => "input",
            HookType::BeforeAgentStart => "before_agent_start",
            HookType::Context => "context",
            HookType::BeforeProviderRequest => "before_provider_request",
            HookType::MessageEnd => "message_end",
            HookType::SessionBeforeCompact => "session_before_compact",
            HookType::SessionBeforeTree => "session_before_tree",
        }
    }

    #[must_use]
    pub fn from_name(name: &str) -> Option<Self> {
        Some(match name {
            "tool_call" => HookType::ToolCall,
            "user_bash" => HookType::UserBash,
            "tool_result" => HookType::ToolResult,
            "input" => HookType::Input,
            "before_agent_start" => HookType::BeforeAgentStart,
            "context" => HookType::Context,
            "before_provider_request" => HookType::BeforeProviderRequest,
            "message_end" => HookType::MessageEnd,
            "session_before_compact" => HookType::SessionBeforeCompact,
            "session_before_tree" => HookType::SessionBeforeTree,
            _ => return None,
        })
    }

    /// Fail-closed hooks (`tool_call`, `user_bash`) block the operation when an
    /// extension times out or crashes. Everything else fails open (proceeds).
    #[must_use]
    pub fn fail_closed(self) -> bool {
        matches!(self, HookType::ToolCall | HookType::UserBash)
    }

    /// Default hook timeout: 60s for fail-closed (they gate execution), 5s for
    /// fail-open. Manifest `hook_timeout_ms` overrides this.
    #[must_use]
    pub fn default_timeout(self) -> Duration {
        if self.fail_closed() {
            Duration::from_secs(60)
        } else {
            Duration::from_secs(5)
        }
    }
}

/// One extension's reply within a hook chain, as seen by the fold.
#[derive(Debug, Clone)]
pub enum HookStep {
    /// The extension replied with this outcome.
    Replied(HookOutcome),
    /// The extension timed out or crashed.
    Failed,
}

/// The folded result of a whole hook chain.
#[derive(Debug, Clone, PartialEq)]
pub enum FoldedHook {
    /// Proceed with this (possibly modified) input value.
    Proceed(Value),
    /// The operation was vetoed, with a reason.
    Blocked(String),
}

/// Fold a hook chain over `input`, in load order. `steps` are the per-extension
/// results in that order. This is the security-critical policy:
///
/// - `Continue` → value unchanged, next extension sees it.
/// - `Modify` → value replaced by the patch, next extension sees the patch.
/// - `Block` → short-circuit; the operation is vetoed (honored for every hook).
/// - `Failed` → for a fail-closed hook, block; for a fail-open hook, proceed
///   unchanged.
#[must_use]
pub fn fold_hook_chain(hook: HookType, input: Value, steps: &[HookStep]) -> FoldedHook {
    let mut current = input;
    for step in steps {
        match step {
            HookStep::Replied(HookOutcome::Continue) => {}
            HookStep::Replied(HookOutcome::Modify { patch }) => current = patch.clone(),
            HookStep::Replied(HookOutcome::Block { reason }) => {
                return FoldedHook::Blocked(reason.clone().unwrap_or_else(|| format!("blocked by {} hook", hook.as_str())));
            }
            HookStep::Failed => {
                if hook.fail_closed() {
                    return FoldedHook::Blocked(format!("{} hook failed (fail-closed)", hook.as_str()));
                }
                // fail-open: proceed with the current value.
            }
        }
    }
    FoldedHook::Proceed(current)
}

// ---------------------------------------------------------------------------
// Host delegate: the ext→host seam (ui / kv / exec / trust).
// ---------------------------------------------------------------------------

/// The host's side of ext→host requests. The engine ships headless defaults;
/// frontends (smooth-code, the daemon, the servers) supply richer impls.
#[async_trait]
pub trait HostDelegate: Send + Sync {
    /// Answer a `ui/request`. Headless default: no UI available.
    async fn ui_request(&self, ext: &str, params: Value) -> Result<Value, RpcError> {
        let _ = (ext, params);
        Err(RpcError::new(codes::NO_UI, "no UI available (headless host)"))
    }

    /// `kv/get`. Default: JSON file per extension.
    async fn kv_get(&self, ext: &str, key: &str) -> Result<Value, RpcError> {
        Ok(kv_file_load(ext).get(key).cloned().unwrap_or(Value::Null))
    }

    /// `kv/set`. Default: JSON file per extension.
    async fn kv_set(&self, ext: &str, key: &str, value: Value) -> Result<(), RpcError> {
        let mut map = kv_file_load(ext);
        map.insert(key.to_string(), value);
        kv_file_store(ext, &map)
    }

    /// `exec/run`. Headless default: deny (no audited permission engine here).
    async fn exec_run(&self, ext: &str, params: Value) -> Result<Value, RpcError> {
        let _ = (ext, params);
        Err(RpcError::new(codes::NOT_TRUSTED, "exec/run is not permitted on the headless host"))
    }
}

/// The engine's headless delegate: NoUI, JSON-file kv, exec denied.
#[derive(Debug, Default)]
pub struct DefaultHostDelegate;

impl HostDelegate for DefaultHostDelegate {}

/// Per-extension kv state file: `$SMOOTH_HOME/extensions/<name>/state.json` (or
/// `~/.smooth/extensions/<name>/state.json`). Kept dependency-free — a flat
/// JSON object.
fn kv_file_path(ext: &str) -> Option<std::path::PathBuf> {
    super::manifest::default_global_dir().map(|d| d.join(ext).join("state.json"))
}

fn kv_file_load(ext: &str) -> serde_json::Map<String, Value> {
    let Some(path) = kv_file_path(ext) else { return serde_json::Map::new() };
    let Ok(text) = std::fs::read_to_string(path) else {
        return serde_json::Map::new();
    };
    serde_json::from_str(&text).unwrap_or_default()
}

fn kv_file_store(ext: &str, map: &serde_json::Map<String, Value>) -> Result<(), RpcError> {
    let Some(path) = kv_file_path(ext) else {
        return Err(RpcError::new(codes::INTERNAL_ERROR, "no home dir for kv store"));
    };
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent).map_err(|e| RpcError::new(codes::INTERNAL_ERROR, format!("kv mkdir: {e}")))?;
    }
    let text = serde_json::to_string_pretty(map).map_err(|e| RpcError::new(codes::INTERNAL_ERROR, format!("kv serialize: {e}")))?;
    std::fs::write(&path, text).map_err(|e| RpcError::new(codes::INTERNAL_ERROR, format!("kv write: {e}")))
}

/// Bridges the process reader's ext→host requests to the [`HostDelegate`].
struct HostInbound {
    ext: String,
    delegate: Arc<dyn HostDelegate>,
}

#[async_trait]
impl InboundHandler for HostInbound {
    async fn handle_request(&self, method_name: &str, params: Value) -> Result<Value, RpcError> {
        match method_name {
            method::PING => Ok(json!({})),
            method::UI_REQUEST => self.delegate.ui_request(&self.ext, params).await,
            method::EXEC_RUN => self.delegate.exec_run(&self.ext, params).await,
            "kv/get" => {
                let key = params.get("key").and_then(Value::as_str).unwrap_or_default();
                Ok(json!({ "value": self.delegate.kv_get(&self.ext, key).await? }))
            }
            "kv/set" => {
                let key = params.get("key").and_then(Value::as_str).unwrap_or_default().to_string();
                let value = params.get("value").cloned().unwrap_or(Value::Null);
                self.delegate.kv_set(&self.ext, &key, value).await?;
                Ok(json!({}))
            }
            other => Err(RpcError::new(codes::METHOD_NOT_FOUND, format!("method not found: {other}"))),
        }
    }

    fn handle_notification(&self, method_name: &str, params: Value) {
        tracing::trace!(ext = %self.ext, method = %method_name, ?params, "ext→host notification");
    }
}

// ---------------------------------------------------------------------------
// ExtensionHost
// ---------------------------------------------------------------------------

/// A loaded, initialized extension.
struct Loaded {
    name: String,
    process: Arc<ExtensionProcess>,
    init: InitializeResult,
    subscriptions: HashSet<String>,
    hook_timeout: Option<Duration>,
}

/// Orchestrates the set of loaded extensions in load order.
pub struct ExtensionHost {
    extensions: Vec<Loaded>,
    epoch: AtomicU64,
}

impl std::fmt::Debug for ExtensionHost {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ExtensionHost")
            .field("extensions", &self.extensions.iter().map(|e| &e.name).collect::<Vec<_>>())
            .finish()
    }
}

impl ExtensionHost {
    /// Load and initialize each discovered extension. Per-extension failures
    /// (spawn, handshake) are tolerated and returned alongside the host. In an
    /// untrusted workspace, project-scoped extensions are skipped.
    pub async fn load(
        discovered: Vec<DiscoveredExtension>,
        host: HostInfo,
        workspace: WorkspaceInfo,
        mode: &str,
        delegate: Arc<dyn HostDelegate>,
    ) -> (Self, Vec<(String, String)>) {
        let mut extensions = Vec::new();
        let mut failures = Vec::new();

        for ext in discovered {
            let name = ext.manifest.name.clone();
            if ext.manifest.disabled {
                continue;
            }
            if ext.scope == Scope::Project && !workspace.trusted {
                tracing::info!(%name, "extension: skipping project extension in untrusted workspace");
                continue;
            }

            match Self::load_one(&ext, &host, &workspace, mode, &delegate).await {
                Ok(loaded) => extensions.push(loaded),
                Err(e) => {
                    tracing::warn!(%name, error = %e, "extension: failed to load");
                    failures.push((name, e.to_string()));
                }
            }
        }

        (
            Self {
                extensions,
                epoch: AtomicU64::new(1),
            },
            failures,
        )
    }

    async fn load_one(
        ext: &DiscoveredExtension,
        host: &HostInfo,
        workspace: &WorkspaceInfo,
        mode: &str,
        delegate: &Arc<dyn HostDelegate>,
    ) -> anyhow::Result<Loaded> {
        let spec = SpawnSpec {
            command: ext.manifest.run.command.clone(),
            args: ext.manifest.run.args.clone(),
            env: ext.manifest.resolved_env(),
            cwd: Some(ext.root.clone()),
        };
        let handler: Arc<dyn InboundHandler> = Arc::new(HostInbound {
            ext: ext.manifest.name.clone(),
            delegate: Arc::clone(delegate),
        });
        let process = Arc::new(ExtensionProcess::spawn(spec, handler)?);

        let params = InitializeParams {
            protocol_version: PROTOCOL_VERSION,
            host: host.clone(),
            workspace: workspace.clone(),
            session: None,
            mode: mode.to_string(),
            ui_capabilities: Vec::new(),
            capabilities_enabled: None,
        };
        let raw = process
            .request(method::INITIALIZE, serde_json::to_value(&params)?, Duration::from_secs(10))
            .await
            .map_err(|e| anyhow::anyhow!("initialize: {e}"))?;
        let init: InitializeResult = serde_json::from_value(raw).map_err(|e| anyhow::anyhow!("bad initialize result: {e}"))?;

        let subscriptions = init.registrations.subscriptions.iter().cloned().collect();
        Ok(Loaded {
            name: ext.manifest.name.clone(),
            process,
            init,
            subscriptions,
            hook_timeout: ext.manifest.hook_timeout_ms.map(Duration::from_millis),
        })
    }

    /// Number of successfully loaded extensions.
    #[must_use]
    pub fn len(&self) -> usize {
        self.extensions.len()
    }

    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.extensions.is_empty()
    }

    /// Names of loaded extensions, in load order.
    #[must_use]
    pub fn names(&self) -> Vec<&str> {
        self.extensions.iter().map(|e| e.name.as_str()).collect()
    }

    /// A fresh dispatch context. Session-mutating actions need `Tier::Command`.
    /// The token embeds the current epoch so it is invalidated across reloads.
    #[must_use]
    pub fn context(&self, tier: Tier) -> Context {
        Context {
            token: format!("epoch-{}", self.epoch.load(Ordering::SeqCst)),
            tier,
        }
    }

    /// Bump the epoch, invalidating every previously minted context token.
    /// Called on reload.
    pub fn bump_epoch(&self) {
        self.epoch.fetch_add(1, Ordering::SeqCst);
    }

    /// True if any loaded extension subscribed to `event`. The host uses this
    /// to skip serialization work when nobody is listening.
    #[must_use]
    pub fn has_subscriber(&self, event: &str) -> bool {
        self.extensions.iter().any(|e| e.subscriptions.contains(event))
    }

    /// Fire-and-forget event fanout to every subscribed extension. Non-blocking:
    /// a slow or dead extension never stalls the caller.
    pub fn dispatch_event(&self, event: &str, payload: Value) {
        if self.extensions.is_empty() {
            return;
        }
        let ctx = self.context(Tier::Event);
        for ext in &self.extensions {
            if !ext.subscriptions.contains(event) {
                continue;
            }
            let params = json!({ "event": event, "context": ctx, "payload": payload });
            if let Err(e) = ext.process.notify(method::EVENT, params) {
                tracing::debug!(ext = %ext.name, error = %e, "extension: event dispatch failed");
            }
        }
    }

    /// Run a hook across every extension in load order, folding the chain. Each
    /// extension sees the prior extension's patch. Fail-open/closed per
    /// [`HookType`].
    pub async fn run_hook(&self, hook: HookType, input: Value) -> FoldedHook {
        if self.extensions.is_empty() {
            return FoldedHook::Proceed(input);
        }
        let ctx = self.context(Tier::Command);
        let mut current = input;

        for ext in &self.extensions {
            let params = json!({ "hook": hook.as_str(), "context": ctx, "input": current });
            let timeout = ext.hook_timeout.unwrap_or_else(|| hook.default_timeout());
            let step = match ext.process.request(method::HOOK, params, timeout).await {
                Ok(value) => match serde_json::from_value::<HookOutcome>(value) {
                    Ok(outcome) => HookStep::Replied(outcome),
                    Err(e) => {
                        tracing::warn!(ext = %ext.name, error = %e, "extension: malformed hook outcome; treating as failure");
                        HookStep::Failed
                    }
                },
                Err(e) => {
                    tracing::warn!(ext = %ext.name, error = %e, "extension: hook call failed");
                    HookStep::Failed
                }
            };

            match fold_hook_chain(hook, current.clone(), std::slice::from_ref(&step)) {
                FoldedHook::Proceed(v) => current = v,
                blocked @ FoldedHook::Blocked(_) => return blocked,
            }
        }
        FoldedHook::Proceed(current)
    }

    /// Convenience: run the `tool_call` hook (fail-closed) on a pending call.
    pub async fn run_tool_call_hook(&self, tool: &str, arguments: &Value) -> FoldedHook {
        self.run_hook(HookType::ToolCall, json!({ "tool": tool, "arguments": arguments })).await
    }

    /// Run the `before_agent_start` hook on a system prompt, returning the
    /// possibly-rewritten prompt. Fail-open: a blocked/failed hook leaves the
    /// prompt unchanged.
    pub async fn before_agent_start(&self, system_prompt: &str) -> String {
        if self.extensions.is_empty() {
            return system_prompt.to_string();
        }
        match self.run_hook(HookType::BeforeAgentStart, json!({ "system_prompt": system_prompt })).await {
            FoldedHook::Proceed(v) => v.get("system_prompt").and_then(Value::as_str).unwrap_or(system_prompt).to_string(),
            FoldedHook::Blocked(_) => system_prompt.to_string(),
        }
    }

    /// Tool proxies for every eager tool every extension registered. Names are
    /// dotted `<ext>.<tool>`; register them via `ToolRegistry::register_arc`.
    /// Deferred tools are returned by [`Self::deferred_tools`].
    #[must_use]
    pub fn tools(&self) -> Vec<Arc<dyn Tool>> {
        self.collect_tools(false)
    }

    /// Deferred tool proxies (register via `ToolRegistry::register_deferred`).
    #[must_use]
    pub fn deferred_tools(&self) -> Vec<Arc<dyn Tool>> {
        self.collect_tools(true)
    }

    fn collect_tools(&self, deferred: bool) -> Vec<Arc<dyn Tool>> {
        let ctx = self.context(Tier::Command);
        let mut out: Vec<Arc<dyn Tool>> = Vec::new();
        for ext in &self.extensions {
            for reg in &ext.init.registrations.tools {
                if reg.deferred != deferred {
                    continue;
                }
                out.push(Arc::new(ExtensionTool::new(&ext.name, reg, Arc::clone(&ext.process), ctx.clone())));
            }
        }
        out
    }

    /// Gracefully shut down every extension (5s grace each, then SIGKILL).
    pub async fn shutdown_all(&self) {
        for ext in &self.extensions {
            ext.process.shutdown(Duration::from_secs(5)).await;
        }
    }
}

/// An empty host: no extensions, every hook a passthrough. Used as the
/// zero-cost default when no extensions are configured.
impl Default for ExtensionHost {
    fn default() -> Self {
        Self {
            extensions: Vec::new(),
            epoch: AtomicU64::new(1),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A host with no extensions — the zero-behavior-change default.
    fn empty_host() -> ExtensionHost {
        ExtensionHost::default()
    }

    #[test]
    fn hook_type_fail_policy_and_timeout() {
        assert!(HookType::ToolCall.fail_closed());
        assert!(HookType::UserBash.fail_closed());
        assert!(!HookType::ToolResult.fail_closed());
        assert!(!HookType::MessageEnd.fail_closed());
        assert_eq!(HookType::ToolCall.default_timeout(), Duration::from_secs(60));
        assert_eq!(HookType::ToolResult.default_timeout(), Duration::from_secs(5));
        assert_eq!(HookType::from_name("before_agent_start"), Some(HookType::BeforeAgentStart));
        assert_eq!(HookType::from_name("nope"), None);
    }

    // ---- fold_hook_chain: the security-critical policy, exhaustively ----

    #[test]
    fn fold_empty_chain_proceeds_unchanged() {
        let input = json!({"tool": "rm"});
        assert_eq!(fold_hook_chain(HookType::ToolCall, input.clone(), &[]), FoldedHook::Proceed(input));
    }

    #[test]
    fn fold_continue_keeps_value() {
        let steps = [HookStep::Replied(HookOutcome::Continue), HookStep::Replied(HookOutcome::Continue)];
        assert_eq!(
            fold_hook_chain(HookType::ToolResult, json!({"a": 1}), &steps),
            FoldedHook::Proceed(json!({"a": 1}))
        );
    }

    #[test]
    fn fold_modify_threads_patch_to_next() {
        // First extension modifies; the fold carries the patch forward.
        let steps = [
            HookStep::Replied(HookOutcome::Modify { patch: json!({"a": 2}) }),
            HookStep::Replied(HookOutcome::Continue),
        ];
        assert_eq!(
            fold_hook_chain(HookType::Context, json!({"a": 1}), &steps),
            FoldedHook::Proceed(json!({"a": 2}))
        );
    }

    #[test]
    fn fold_block_short_circuits() {
        let steps = [
            HookStep::Replied(HookOutcome::Block {
                reason: Some("rm -rf blocked".into()),
            }),
            HookStep::Replied(HookOutcome::Modify {
                patch: json!({"should": "not apply"}),
            }),
        ];
        assert_eq!(
            fold_hook_chain(HookType::ToolCall, json!({}), &steps),
            FoldedHook::Blocked("rm -rf blocked".into())
        );
    }

    #[test]
    fn fold_block_without_reason_gets_default() {
        let steps = [HookStep::Replied(HookOutcome::Block { reason: None })];
        assert_eq!(
            fold_hook_chain(HookType::UserBash, json!({}), &steps),
            FoldedHook::Blocked("blocked by user_bash hook".into())
        );
    }

    #[test]
    fn fold_failure_is_fail_closed_for_tool_call() {
        // A crashed/timed-out extension BLOCKS a fail-closed hook.
        let steps = [HookStep::Failed];
        match fold_hook_chain(HookType::ToolCall, json!({}), &steps) {
            FoldedHook::Blocked(msg) => assert!(msg.contains("fail-closed")),
            other => panic!("expected Blocked, got {other:?}"),
        }
    }

    #[test]
    fn fold_failure_is_fail_open_for_others() {
        // A crashed extension does NOT block a fail-open hook; the value passes.
        let steps = [HookStep::Failed, HookStep::Replied(HookOutcome::Continue)];
        assert_eq!(
            fold_hook_chain(HookType::ToolResult, json!({"x": 9}), &steps),
            FoldedHook::Proceed(json!({"x": 9}))
        );
    }

    #[test]
    fn fold_modify_then_failure_fail_open_keeps_patch() {
        let steps = [HookStep::Replied(HookOutcome::Modify { patch: json!({"x": 2}) }), HookStep::Failed];
        assert_eq!(fold_hook_chain(HookType::Input, json!({"x": 1}), &steps), FoldedHook::Proceed(json!({"x": 2})));
    }

    // ---- HostDelegate defaults ----

    #[tokio::test]
    async fn default_delegate_ui_is_no_ui() {
        let d = DefaultHostDelegate;
        let err = d.ui_request("ext", json!({"kind": "confirm"})).await.unwrap_err();
        assert_eq!(err.code, codes::NO_UI);
    }

    #[tokio::test]
    async fn default_delegate_exec_denied() {
        let d = DefaultHostDelegate;
        let err = d.exec_run("ext", json!({"command": "ls"})).await.unwrap_err();
        assert_eq!(err.code, codes::NOT_TRUSTED);
    }

    // The kv default reads `SMOOTH_HOME` (process-global), and the HostInbound
    // kv routing shares it — kept in ONE test so the env mutation can't race a
    // sibling test running in parallel.
    #[tokio::test]
    async fn default_delegate_and_host_inbound_kv() {
        let tmp = tempfile::tempdir().unwrap();
        std::env::set_var("SMOOTH_HOME", tmp.path());

        // Direct delegate round-trip.
        let d = DefaultHostDelegate;
        assert_eq!(d.kv_get("kvtest", "missing").await.unwrap(), Value::Null);
        d.kv_set("kvtest", "k", json!({"n": 1})).await.unwrap();
        assert_eq!(d.kv_get("kvtest", "k").await.unwrap(), json!({"n": 1}));

        // Routed through HostInbound (ext→host bridge).
        let inbound = HostInbound {
            ext: "e".into(),
            delegate: Arc::new(DefaultHostDelegate),
        };
        assert!(inbound.handle_request(method::PING, Value::Null).await.is_ok());
        inbound.handle_request("kv/set", json!({"key": "a", "value": 5})).await.unwrap();
        let got = inbound.handle_request("kv/get", json!({"key": "a"})).await.unwrap();
        assert_eq!(got, json!({"value": 5}));
        let err = inbound.handle_request("nope/method", Value::Null).await.unwrap_err();
        assert_eq!(err.code, codes::METHOD_NOT_FOUND);

        std::env::remove_var("SMOOTH_HOME");
    }

    // ---- empty host: the zero-behavior-change default ----

    #[tokio::test]
    async fn empty_host_hook_is_passthrough() {
        let host = empty_host();
        assert!(host.is_empty());
        assert_eq!(
            host.run_hook(HookType::ToolCall, json!({"tool": "x"})).await,
            FoldedHook::Proceed(json!({"tool": "x"}))
        );
        assert_eq!(host.before_agent_start("prompt").await, "prompt");
        assert!(host.tools().is_empty());
        host.dispatch_event("turn_start", json!({})); // no-op, must not panic
    }
}
