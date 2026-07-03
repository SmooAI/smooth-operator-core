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
use super::protocol::{
    codes, method, CommandCompleteResult, CommandExecuteResult, CommandRegistration, Completion, Context, HookOutcome, HostInfo, InitializeParams,
    InitializeResult, RpcError, ShortcutRegistration, Tier, WorkspaceInfo,
};
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

/// Effective event subscriptions: what the extension asked for at handshake,
/// clamped to what its manifest `[capabilities] events` declared. An empty
/// declared list means "no declared filter" → trust the handshake as-is (keeps
/// capability-less test peers working); a non-empty list is the outer bound the
/// extension can never widen past.
#[must_use]
pub fn effective_subscriptions(declared: &[String], requested: &[String]) -> HashSet<String> {
    if declared.is_empty() {
        requested.iter().cloned().collect()
    } else {
        let declared: HashSet<&String> = declared.iter().collect();
        requested.iter().filter(|s| declared.contains(*s)).cloned().collect()
    }
}

/// Parse the epoch embedded in a context token minted by [`ExtensionHost::context`]
/// (`epoch-<N>`). Returns `None` for a malformed token.
#[must_use]
fn token_epoch(token: &str) -> Option<u64> {
    token.strip_prefix("epoch-").and_then(|n| n.parse().ok())
}

/// The two-tier deadlock guard: a session-mutating ext→host action is valid only
/// when it presents a COMMAND-tier context whose epoch is still current. An
/// event-tier context, or a stale token minted before a reload bumped the epoch,
/// is rejected with `-32003 ContextViolation`. This is the security-critical
/// gate — kept a pure function so it can be tested exhaustively.
fn validate_command_context(params: &Value, current_epoch: u64) -> Result<(), RpcError> {
    let ctx = params.get("context");
    let tier = ctx.and_then(|c| c.get("tier")).and_then(Value::as_str);
    if tier != Some("command") {
        return Err(RpcError::new(codes::CONTEXT_VIOLATION, "session action requires a command-tier context"));
    }
    let token = ctx.and_then(|c| c.get("token")).and_then(Value::as_str).unwrap_or_default();
    match token_epoch(token) {
        Some(e) if e == current_epoch => Ok(()),
        _ => Err(RpcError::new(
            codes::CONTEXT_VIOLATION,
            "session action presented a stale context (epoch mismatch)",
        )),
    }
}

// ---------------------------------------------------------------------------
// Host delegate: the ext→host seam (ui / kv / exec / session / trust).
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

    /// `session/send_message` — inject a message into the session transcript.
    /// The context has already been validated (command tier, current epoch)
    /// before this is called. The engine has no session concept, so the default
    /// reports the capability is unavailable; frontends with a session store
    /// (smooth-code, the operative, the daemon) override these three.
    async fn session_send_message(&self, ext: &str, params: Value) -> Result<Value, RpcError> {
        let _ = (ext, params);
        Err(RpcError::new(codes::CAPABILITY_DISABLED, "session actions are unavailable on this host"))
    }

    /// `session/send_user_message` — deliver a user message (steer/follow_up/
    /// next_turn). Context pre-validated. Default: capability unavailable.
    async fn session_send_user_message(&self, ext: &str, params: Value) -> Result<Value, RpcError> {
        let _ = (ext, params);
        Err(RpcError::new(codes::CAPABILITY_DISABLED, "session actions are unavailable on this host"))
    }

    /// `session/append_entry` — append an LLM-invisible transcript entry. Context
    /// pre-validated. Default: capability unavailable.
    async fn session_append_entry(&self, ext: &str, params: Value) -> Result<Value, RpcError> {
        let _ = (ext, params);
        Err(RpcError::new(codes::CAPABILITY_DISABLED, "session actions are unavailable on this host"))
    }

    /// A `tool/update` progress notification streamed by an extension during an
    /// in-flight `tool/execute`, keyed by its `call_id`. Fire-and-forget. The
    /// headless default only traces; a frontend/daemon overrides this to surface
    /// progress (e.g. emit an [`AgentEvent::ToolCallUpdate`](crate::AgentEvent)).
    fn tool_update(&self, ext: &str, params: Value) {
        tracing::trace!(ext = %ext, ?params, "extension: tool/update progress (dropped by headless host)");
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

/// Bridges the process reader's ext→host requests to the [`HostDelegate`]. Holds
/// the host's shared epoch so it can reject stale/event-tier session actions.
struct HostInbound {
    ext: String,
    delegate: Arc<dyn HostDelegate>,
    epoch: Arc<AtomicU64>,
}

#[async_trait]
impl InboundHandler for HostInbound {
    async fn handle_request(&self, method_name: &str, params: Value) -> Result<Value, RpcError> {
        match method_name {
            method::PING => Ok(json!({})),
            method::UI_REQUEST => self.delegate.ui_request(&self.ext, params).await,
            method::EXEC_RUN => self.delegate.exec_run(&self.ext, params).await,
            // Session actions are the tier-guarded set: validate the presented
            // context (command tier + current epoch) BEFORE touching the delegate.
            method::SESSION_SEND_MESSAGE => {
                validate_command_context(&params, self.epoch.load(Ordering::SeqCst))?;
                self.delegate.session_send_message(&self.ext, params).await
            }
            method::SESSION_SEND_USER_MESSAGE => {
                validate_command_context(&params, self.epoch.load(Ordering::SeqCst))?;
                self.delegate.session_send_user_message(&self.ext, params).await
            }
            method::SESSION_APPEND_ENTRY => {
                validate_command_context(&params, self.epoch.load(Ordering::SeqCst))?;
                self.delegate.session_append_entry(&self.ext, params).await
            }
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
        match method_name {
            method::TOOL_UPDATE => self.delegate.tool_update(&self.ext, params),
            other => tracing::trace!(ext = %self.ext, method = %other, ?params, "ext→host notification"),
        }
    }
}

// ---------------------------------------------------------------------------
// ExtensionHost
// ---------------------------------------------------------------------------

/// A loaded, initialized extension. `init` and `subscriptions` are interior-
/// mutable so a hot [`reload`](ExtensionHost::reload) can swap in the freshly
/// re-initialized registrations without disturbing the stable `process` Arc.
struct Loaded {
    name: String,
    process: Arc<ExtensionProcess>,
    init: std::sync::RwLock<InitializeResult>,
    subscriptions: std::sync::RwLock<HashSet<String>>,
    /// The manifest's declared event allow-list — the clamp `subscriptions` can
    /// never widen past, re-applied on reload so a restart can't escalate.
    declared_events: Vec<String>,
    hook_timeout: Option<Duration>,
}

/// Orchestrates the set of loaded extensions in load order.
pub struct ExtensionHost {
    extensions: Vec<Loaded>,
    /// Shared with every [`HostInbound`] so a session-action's context can be
    /// checked against the live epoch (a reload bumps it, invalidating tokens).
    epoch: Arc<AtomicU64>,
    /// The handshake context, retained so [`reload`](Self::reload) can re-send
    /// `initialize` with the same host/workspace/mode/ui_capabilities.
    host: HostInfo,
    workspace: WorkspaceInfo,
    mode: String,
    ui_capabilities: Vec<String>,
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
        ui_capabilities: Vec<String>,
        delegate: Arc<dyn HostDelegate>,
    ) -> (Self, Vec<(String, String)>) {
        let mut extensions = Vec::new();
        let mut failures = Vec::new();
        let epoch = Arc::new(AtomicU64::new(1));

        for ext in discovered {
            let name = ext.manifest.name.clone();
            if ext.manifest.disabled {
                continue;
            }
            if ext.scope == Scope::Project && !workspace.trusted {
                tracing::info!(%name, "extension: skipping project extension in untrusted workspace");
                continue;
            }

            match Self::load_one(&ext, &host, &workspace, mode, &ui_capabilities, &delegate, &epoch).await {
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
                epoch,
                host,
                workspace,
                mode: mode.to_string(),
                ui_capabilities,
            },
            failures,
        )
    }

    async fn load_one(
        ext: &DiscoveredExtension,
        host: &HostInfo,
        workspace: &WorkspaceInfo,
        mode: &str,
        ui_capabilities: &[String],
        delegate: &Arc<dyn HostDelegate>,
        epoch: &Arc<AtomicU64>,
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
            epoch: Arc::clone(epoch),
        });
        let process = Arc::new(ExtensionProcess::spawn(spec, handler)?);

        let init = Self::initialize(&process, host, workspace, mode, ui_capabilities).await?;
        let subscriptions = effective_subscriptions(&ext.manifest.capabilities.events, &init.registrations.subscriptions);
        Ok(Loaded {
            name: ext.manifest.name.clone(),
            process,
            init: std::sync::RwLock::new(init),
            subscriptions: std::sync::RwLock::new(subscriptions),
            declared_events: ext.manifest.capabilities.events.clone(),
            hook_timeout: ext.manifest.hook_timeout_ms.map(Duration::from_millis),
        })
    }

    /// Send the `initialize` handshake to a (possibly freshly respawned) process
    /// and parse the registrations. Shared by initial load and hot reload.
    async fn initialize(
        process: &ExtensionProcess,
        host: &HostInfo,
        workspace: &WorkspaceInfo,
        mode: &str,
        ui_capabilities: &[String],
    ) -> anyhow::Result<InitializeResult> {
        let params = InitializeParams {
            protocol_version: PROTOCOL_VERSION,
            host: host.clone(),
            workspace: workspace.clone(),
            session: None,
            mode: mode.to_string(),
            ui_capabilities: ui_capabilities.to_vec(),
            flags: serde_json::Map::new(),
            capabilities_enabled: None,
        };
        let raw = process
            .request(method::INITIALIZE, serde_json::to_value(&params)?, Duration::from_secs(10))
            .await
            .map_err(|e| anyhow::anyhow!("initialize: {e}"))?;
        serde_json::from_value(raw).map_err(|e| anyhow::anyhow!("bad initialize result: {e}"))
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
        self.extensions
            .iter()
            .any(|e| e.subscriptions.read().expect("subscriptions lock").contains(event))
    }

    /// Fire-and-forget event fanout to every subscribed extension. Non-blocking:
    /// a slow or dead extension never stalls the caller.
    pub fn dispatch_event(&self, event: &str, payload: Value) {
        if self.extensions.is_empty() {
            return;
        }
        let ctx = serde_json::to_value(self.context(Tier::Event)).unwrap_or(Value::Null);
        for ext in &self.extensions {
            if !ext.subscriptions.read().expect("subscriptions lock").contains(event) {
                continue;
            }
            // Bounded, lossy lane: a slow extension sheds oldest events (with an
            // events_lost marker) instead of stalling the agent or leaking memory.
            ext.process.send_event(event, &ctx, payload.clone());
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
            for reg in &ext.init.read().expect("init lock").registrations.tools {
                if reg.deferred != deferred {
                    continue;
                }
                out.push(Arc::new(ExtensionTool::new(&ext.name, reg, Arc::clone(&ext.process), ctx.clone())));
            }
        }
        out
    }

    /// Eager tool proxies for a single extension, minted at the CURRENT epoch.
    /// The frontend calls this after a [`reload`](Self::reload) to re-register
    /// the reloaded extension's tools (its old proxies carry a stale context).
    #[must_use]
    pub fn tools_for(&self, ext_name: &str) -> Vec<Arc<dyn Tool>> {
        let ctx = self.context(Tier::Command);
        let Some(ext) = self.extensions.iter().find(|e| e.name == ext_name) else {
            return Vec::new();
        };
        ext.init
            .read()
            .expect("init lock")
            .registrations
            .tools
            .iter()
            .filter(|reg| !reg.deferred)
            .map(|reg| Arc::new(ExtensionTool::new(&ext.name, reg, Arc::clone(&ext.process), ctx.clone())) as Arc<dyn Tool>)
            .collect()
    }

    /// Every registered slash-command across all extensions, paired with the
    /// owning extension name. Frontends surface these in their `/` command
    /// palette. Command names are namespaced by the frontend (`/<ext>.<cmd>`).
    #[must_use]
    pub fn commands(&self) -> Vec<(String, CommandRegistration)> {
        let mut out = Vec::new();
        for ext in &self.extensions {
            for cmd in &ext.init.read().expect("init lock").registrations.commands {
                out.push((ext.name.clone(), cmd.clone()));
            }
        }
        out
    }

    /// Every keyboard shortcut across all extensions, paired with the owning
    /// extension name. Only frontends with a key surface (the TUI) honor these.
    #[must_use]
    pub fn shortcuts(&self) -> Vec<(String, ShortcutRegistration)> {
        let mut out = Vec::new();
        for ext in &self.extensions {
            for sc in &ext.init.read().expect("init lock").registrations.shortcuts {
                out.push((ext.name.clone(), sc.clone()));
            }
        }
        out
    }

    /// Find the extension process that registered `command` (optionally scoped to
    /// a specific extension when the name was namespaced `<ext>.<cmd>`).
    fn command_owner(&self, ext_name: Option<&str>, command: &str) -> Option<Arc<ExtensionProcess>> {
        for ext in &self.extensions {
            if ext_name.is_some_and(|n| n != ext.name) {
                continue;
            }
            if ext.init.read().expect("init lock").registrations.commands.iter().any(|c| c.name == command) {
                return Some(Arc::clone(&ext.process));
            }
        }
        None
    }

    /// Dispatch a registered slash-command to its owning extension with a
    /// COMMAND-tier context (so the handler may take session actions). Pass
    /// `ext_name` to disambiguate a command registered by more than one
    /// extension; `None` picks the first match in load order.
    ///
    /// # Errors
    /// `-32601` if no loaded extension registered `command`; otherwise the
    /// extension's own error.
    pub async fn run_command(&self, ext_name: Option<&str>, command: &str, arguments: Value) -> Result<CommandExecuteResult, RpcError> {
        let process = self
            .command_owner(ext_name, command)
            .ok_or_else(|| RpcError::new(codes::METHOD_NOT_FOUND, format!("no extension registered command `{command}`")))?;
        let params = json!({ "command": command, "context": self.context(Tier::Command), "arguments": arguments });
        let raw = process
            .request(method::COMMAND_EXECUTE, params, Duration::from_secs(120))
            .await
            .map_err(|e| RpcError::new(codes::INTERNAL_ERROR, format!("command/execute: {e}")))?;
        serde_json::from_value(raw).map_err(|e| RpcError::new(codes::INTERNAL_ERROR, format!("bad command/execute result: {e}")))
    }

    /// Ask the extension that owns `command` for argument completions given the
    /// `partial` text typed so far. Returns an empty list when the extension does
    /// not implement completion or replies with an error (autocomplete is
    /// best-effort — never fail the caller's keystroke).
    pub async fn complete_command(&self, ext_name: Option<&str>, command: &str, partial: &str) -> Vec<Completion> {
        let Some(process) = self.command_owner(ext_name, command) else {
            return Vec::new();
        };
        let params = json!({ "command": command, "context": self.context(Tier::Command), "partial": partial });
        match process.request(method::COMMAND_COMPLETE, params, Duration::from_secs(5)).await {
            Ok(raw) => serde_json::from_value::<CommandCompleteResult>(raw).map(|r| r.completions).unwrap_or_default(),
            Err(e) => {
                tracing::trace!(%command, error = %e, "extension: command/complete failed; no completions");
                Vec::new()
            }
        }
    }

    /// Hot-reload a single extension by name: notify it (`session_shutdown`
    /// reason `reload`), bump the epoch so every context token it still holds is
    /// invalidated, respawn its subprocess (the generation guard discards any
    /// late reply from the dead child), re-run `initialize` to pick up its new
    /// registrations, then notify it (`session_start` reason `reload`). The
    /// caller re-registers the extension's tools via [`tools_for`](Self::tools_for)
    /// (old proxies carry the pre-bump context). No-op error if `name` is not
    /// loaded.
    ///
    /// # Errors
    /// Propagates a respawn or re-initialize failure. On failure the extension is
    /// left dead — reload is not atomic, but the epoch bump already fenced off
    /// stale contexts.
    pub async fn reload(&self, name: &str) -> anyhow::Result<()> {
        let Some(ext) = self.extensions.iter().find(|e| e.name == name) else {
            anyhow::bail!("extension `{name}` is not loaded");
        };
        // Best-effort lifecycle notice to the outgoing generation.
        let reload_ctx = serde_json::to_value(self.context(Tier::Event)).unwrap_or(Value::Null);
        ext.process.send_event("session_shutdown", &reload_ctx, json!({ "reason": "reload" }));

        // Fence: any context token minted before this point is now stale.
        self.bump_epoch();
        ext.process.respawn()?;

        let init = Self::initialize(&ext.process, &self.host, &self.workspace, &self.mode, &self.ui_capabilities).await?;
        let subs = effective_subscriptions(&ext.declared_events, &init.registrations.subscriptions);
        *ext.subscriptions.write().expect("subscriptions lock") = subs;
        *ext.init.write().expect("init lock") = init;

        let start_ctx = serde_json::to_value(self.context(Tier::Event)).unwrap_or(Value::Null);
        ext.process.send_event("session_start", &start_ctx, json!({ "reason": "reload" }));
        Ok(())
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
            epoch: Arc::new(AtomicU64::new(1)),
            host: HostInfo {
                name: "smooth-operator-core".into(),
                version: env!("CARGO_PKG_VERSION").into(),
            },
            workspace: WorkspaceInfo {
                root: String::new(),
                trusted: false,
            },
            mode: "headless".into(),
            ui_capabilities: Vec::new(),
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
    fn effective_subscriptions_intersects_or_passes_through() {
        let s = |xs: &[&str]| xs.iter().map(|x| (*x).to_string()).collect::<Vec<_>>();
        // No declared filter → handshake as-is.
        assert_eq!(
            effective_subscriptions(&[], &s(&["turn_start", "turn_end"])),
            HashSet::from(["turn_start".to_string(), "turn_end".to_string()])
        );
        // Declared list clamps: `tool_call` requested but not declared is dropped.
        assert_eq!(
            effective_subscriptions(&s(&["turn_start"]), &s(&["turn_start", "tool_call"])),
            HashSet::from(["turn_start".to_string()])
        );
        // Declared but not requested → not subscribed.
        assert!(effective_subscriptions(&s(&["turn_start", "turn_end"]), &s(&["turn_end"])).len() == 1);
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
            epoch: Arc::new(AtomicU64::new(1)),
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
        assert!(host.commands().is_empty());
        assert!(host.shortcuts().is_empty());
    }

    // ---- the command-tier deadlock guard (security-critical), exhaustively ----

    #[test]
    fn token_epoch_parses_or_none() {
        assert_eq!(token_epoch("epoch-7"), Some(7));
        assert_eq!(token_epoch("epoch-0"), Some(0));
        assert_eq!(token_epoch("epoch-"), None);
        assert_eq!(token_epoch("7"), None);
        assert_eq!(token_epoch("nonce-3"), None);
    }

    fn ctx(tier: &str, token: &str) -> Value {
        json!({ "context": { "tier": tier, "token": token }, "text": "hi" })
    }

    #[test]
    fn validate_command_context_accepts_current_command_tier() {
        assert!(validate_command_context(&ctx("command", "epoch-4"), 4).is_ok());
    }

    #[test]
    fn validate_command_context_rejects_event_tier() {
        let e = validate_command_context(&ctx("event", "epoch-4"), 4).unwrap_err();
        assert_eq!(e.code, codes::CONTEXT_VIOLATION);
    }

    #[test]
    fn validate_command_context_rejects_stale_epoch() {
        // A token minted at epoch 4, checked after a reload bumped the host to 5.
        let e = validate_command_context(&ctx("command", "epoch-4"), 5).unwrap_err();
        assert_eq!(e.code, codes::CONTEXT_VIOLATION);
    }

    #[test]
    fn validate_command_context_rejects_missing_and_malformed() {
        assert_eq!(validate_command_context(&json!({"text": "hi"}), 1).unwrap_err().code, codes::CONTEXT_VIOLATION);
        assert_eq!(
            validate_command_context(&ctx("command", "garbage"), 1).unwrap_err().code,
            codes::CONTEXT_VIOLATION
        );
    }

    /// A delegate that records which session action fired.
    #[derive(Default)]
    struct RecordingDelegate {
        hits: std::sync::Mutex<Vec<String>>,
    }

    #[async_trait]
    impl HostDelegate for RecordingDelegate {
        async fn session_send_message(&self, _ext: &str, _params: Value) -> Result<Value, RpcError> {
            self.hits.lock().unwrap().push("send_message".into());
            Ok(json!({}))
        }
        async fn session_append_entry(&self, _ext: &str, _params: Value) -> Result<Value, RpcError> {
            self.hits.lock().unwrap().push("append_entry".into());
            Ok(json!({}))
        }
    }

    #[tokio::test]
    async fn host_inbound_session_action_validates_before_delegate() {
        let delegate = Arc::new(RecordingDelegate::default());
        let epoch = Arc::new(AtomicU64::new(3));
        let inbound = HostInbound {
            ext: "e".into(),
            delegate: Arc::clone(&delegate) as Arc<dyn HostDelegate>,
            epoch: Arc::clone(&epoch),
        };

        // Valid: command tier + current epoch → delegate is hit.
        inbound
            .handle_request(method::SESSION_SEND_MESSAGE, ctx("command", "epoch-3"))
            .await
            .expect("valid command context should pass");
        assert_eq!(&*delegate.hits.lock().unwrap(), &["send_message"]);

        // Event-tier → -32003 BEFORE the delegate (no new hit recorded).
        let err = inbound.handle_request(method::SESSION_APPEND_ENTRY, ctx("event", "epoch-3")).await.unwrap_err();
        assert_eq!(err.code, codes::CONTEXT_VIOLATION);

        // Stale epoch (a reload bumped 3→4) → -32003, delegate untouched.
        epoch.store(4, Ordering::SeqCst);
        let err = inbound
            .handle_request(method::SESSION_SEND_MESSAGE, ctx("command", "epoch-3"))
            .await
            .unwrap_err();
        assert_eq!(err.code, codes::CONTEXT_VIOLATION);

        // Only the one valid call ever reached the delegate.
        assert_eq!(&*delegate.hits.lock().unwrap(), &["send_message"]);
    }
}
