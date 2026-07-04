//! Persistent permission grants — `wonk-allow.toml` (pearl th-22bfc1).
//!
//! The [`PermissionHook`](crate::permission::PermissionHook) gate closes on an
//! `Ask` verdict by prompting a human. Without persistence that prompt is
//! *approve-once*: the same command re-asks on every run. This module ports
//! smooth's `wonk-allow.toml` allow-list so a human's "approve always" answer
//! is remembered — a stored grant that matches a later `Ask` auto-approves it
//! **without prompting**.
//!
//! Two TOML files are stacked at load time (project wins on collision):
//!
//! - `~/.smooth/wonk-allow.toml` — the user's personal grants.
//! - `<repo>/.smooth/wonk-allow.toml` — project-scoped grants (checked into git
//!   so teammates inherit the approvals).
//!
//! The on-disk schema is compatible in spirit with smooth's
//! `smooth-bigsmooth::wonk_grants` (same filename, same section names) so the
//! files interoperate; this crate cannot depend on smooth (smooth depends on
//! *it*), hence the port.
//!
//! ## Schema (v1)
//!
//! ```toml
//! schema_version = 1
//!
//! [network]
//! allow_hosts = ["api.openai.com", "*.openai.com"]
//!
//! [tools]
//! allow = ["web_search", "vendor.file_write"]
//!
//! [bash]
//! allow_patterns = ["cargo ", "pnpm "]
//! ```
//!
//! - `network.allow_hosts` — exact host or `*.suffix` glob (case-insensitive).
//! - `tools.allow` — exact tool name (writes / unknown tools grant by name).
//! - `bash.allow_patterns` — a command *prefix*; the trailing space in
//!   `"cargo "` is significant (stops it matching `cargonaut`).
//!
//! There is no deny section: a stored grant can only upgrade an `Ask`, **never**
//! waive a `Deny` circuit-breaker (see [`crate::permission`]).
//!
//! ## Robustness
//!
//! - Missing file → empty store (first run needs no `touch`).
//! - Malformed file → error surfaced (not silently ignored) so a corrupt
//!   allow-list fails loud rather than silently granting nothing / everything.
//! - Writes are atomic (write-to-tempfile-then-rename): a crash mid-save leaves
//!   the previous file intact.

use std::collections::BTreeSet;
use std::path::{Path, PathBuf};
use std::sync::{Arc, RwLock};

use serde::{Deserialize, Serialize};

/// In-memory snapshot of `wonk-allow.toml`.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct PermissionGrants {
    /// Always 1. Reserved for forward-compatible migrations.
    pub schema_version: u32,
    #[serde(skip_serializing_if = "NetworkSection::is_empty", default)]
    pub network: NetworkSection,
    #[serde(skip_serializing_if = "ToolsSection::is_empty", default)]
    pub tools: ToolsSection,
    #[serde(skip_serializing_if = "BashSection::is_empty", default)]
    pub bash: BashSection,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct NetworkSection {
    /// Hosts (or `*.suffix` globs) approved without asking.
    pub allow_hosts: BTreeSet<String>,
}

impl NetworkSection {
    fn is_empty(&self) -> bool {
        self.allow_hosts.is_empty()
    }
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct ToolsSection {
    /// Tool names approved without asking. Exact match only.
    pub allow: BTreeSet<String>,
}

impl ToolsSection {
    fn is_empty(&self) -> bool {
        self.allow.is_empty()
    }
}

#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct BashSection {
    /// Command prefixes approved without asking. `"cargo "` matches
    /// `cargo test`, `cargo build`, … — the trailing space is the guard
    /// against `cargonaut`.
    pub allow_patterns: BTreeSet<String>,
}

impl BashSection {
    fn is_empty(&self) -> bool {
        self.allow_patterns.is_empty()
    }
}

/// The kind of resource a grant covers — one of the three grantable `Ask`
/// shapes. (`Deny` circuit-breakers are never grantable.)
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum GrantQuery {
    /// A network host (or `*.suffix` glob).
    Network(String),
    /// An exact tool name (write / unknown tool).
    Tool(String),
    /// A bash command prefix, e.g. `"npm "`.
    Bash(String),
}

impl PermissionGrants {
    /// New grants pinned at the current schema version.
    #[must_use]
    pub fn new() -> Self {
        Self {
            schema_version: 1,
            ..Self::default()
        }
    }

    /// True if `host` is covered by the `[network]` allow-list.
    #[must_use]
    pub fn matches_host(&self, host: &str) -> bool {
        let lower = host.to_ascii_lowercase();
        self.network.allow_hosts.iter().any(|pat| host_matches_glob(&lower, pat))
    }

    /// True if `tool_name` is in the `[tools]` allow-list (exact match).
    #[must_use]
    pub fn matches_tool(&self, tool_name: &str) -> bool {
        self.tools.allow.contains(tool_name)
    }

    /// True if `command` starts with any `[bash]` allow prefix.
    #[must_use]
    pub fn matches_bash(&self, command: &str) -> bool {
        let lower = command.to_ascii_lowercase();
        self.bash.allow_patterns.iter().any(|p| lower.starts_with(&p.to_ascii_lowercase()))
    }

    /// True if `query`'s exact entry is already stored (used to decide whether
    /// approve-always needs to persist anything).
    #[must_use]
    pub fn contains(&self, query: &GrantQuery) -> bool {
        match query {
            GrantQuery::Network(h) => self.matches_host(h),
            GrantQuery::Tool(t) => self.matches_tool(t),
            GrantQuery::Bash(p) => self.matches_bash(p),
        }
    }

    /// Add a grant. Idempotent.
    pub fn add(&mut self, query: GrantQuery) {
        match query {
            GrantQuery::Network(h) => {
                self.network.allow_hosts.insert(h);
            }
            GrantQuery::Tool(t) => {
                self.tools.allow.insert(t);
            }
            GrantQuery::Bash(p) => {
                self.bash.allow_patterns.insert(p);
            }
        }
    }

    /// Union `other` into `self`.
    pub fn merge_with(&mut self, other: PermissionGrants) {
        self.schema_version = self.schema_version.max(other.schema_version);
        self.network.allow_hosts.extend(other.network.allow_hosts);
        self.tools.allow.extend(other.tools.allow);
        self.bash.allow_patterns.extend(other.bash.allow_patterns);
    }

    /// Parse from a TOML string. Missing sections default to empty.
    ///
    /// # Errors
    /// Returns the TOML parse error if the input isn't valid v1.
    pub fn parse(toml_text: &str) -> anyhow::Result<Self> {
        Ok(toml::from_str(toml_text)?)
    }

    /// Serialize to pretty TOML.
    ///
    /// # Errors
    /// Propagates `toml::ser::Error`.
    pub fn to_toml_string(&self) -> anyhow::Result<String> {
        Ok(toml::to_string_pretty(self)?)
    }

    /// Load from `path`. A missing file yields an empty (v1) store — **not**
    /// an error. A malformed file surfaces the parse error.
    ///
    /// # Errors
    /// I/O errors other than `NotFound`, and TOML parse errors.
    pub fn load_from_path(path: &Path) -> anyhow::Result<Self> {
        match std::fs::read_to_string(path) {
            Ok(text) => Self::parse(&text).map_err(|e| anyhow::anyhow!("malformed wonk-allow.toml at {}: {e}", path.display())),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(Self::new()),
            Err(e) => Err(e.into()),
        }
    }

    /// Load user + project files and merge them (**project wins** on collision —
    /// though for pure union allow-lists "wins" only affects `schema_version`).
    /// Either path missing is fine; a malformed file present is an error.
    ///
    /// # Errors
    /// Surfaces a malformed file at either path.
    pub fn load_layered(user: Option<&Path>, project: Option<&Path>) -> anyhow::Result<Self> {
        let mut merged = PermissionGrants::new();
        if let Some(u) = user {
            merged.merge_with(Self::load_from_path(u)?);
        }
        if let Some(p) = project {
            // Project last so its schema_version wins; entries union either way.
            merged.merge_with(Self::load_from_path(p)?);
        }
        Ok(merged)
    }

    /// Atomically write to `path` (tempfile + rename), creating parent dirs.
    ///
    /// # Errors
    /// I/O and TOML serialization errors.
    pub fn save_to_path(&self, path: &Path) -> anyhow::Result<()> {
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)?;
        }
        let text = self.to_toml_string()?;
        let tmp_path = path.with_extension("toml.tmp");
        std::fs::write(&tmp_path, text)?;
        std::fs::rename(&tmp_path, path)?;
        Ok(())
    }
}

/// The user-scope grants file: `~/.smooth/wonk-allow.toml`. `None` when there
/// is no home dir (minimal CI / broken containers).
#[must_use]
pub fn user_grants_path() -> Option<PathBuf> {
    dirs_next::home_dir().map(|h| h.join(".smooth").join("wonk-allow.toml"))
}

/// The project-scope grants file: `<workspace>/.smooth/wonk-allow.toml`.
#[must_use]
pub fn project_grants_path(workspace: &Path) -> PathBuf {
    workspace.join(".smooth").join("wonk-allow.toml")
}

/// Load the grant at `path`, add `query`, and atomically save. Creates the file
/// if absent. Idempotent for a query that's already stored.
///
/// # Errors
/// I/O and TOML parse/serialize errors.
pub fn append_grant(path: &Path, query: GrantQuery) -> anyhow::Result<()> {
    let mut grants = PermissionGrants::load_from_path(path)?;
    if grants.schema_version == 0 {
        grants.schema_version = 1;
    }
    grants.add(query);
    grants.save_to_path(path)?;
    Ok(())
}

/// Thread-safe, cheaply-cloned handle to the live merged grants. Reads take a
/// snapshot; approve-always merges the freshly-persisted grant back in.
#[derive(Debug, Clone, Default)]
pub struct SharedGrants {
    inner: Arc<RwLock<PermissionGrants>>,
}

impl SharedGrants {
    #[must_use]
    pub fn new(grants: PermissionGrants) -> Self {
        Self {
            inner: Arc::new(RwLock::new(grants)),
        }
    }

    /// A lock-free (cloned-out) snapshot for matching.
    #[must_use]
    pub fn snapshot(&self) -> PermissionGrants {
        self.inner.read().map(|g| g.clone()).unwrap_or_default()
    }

    /// Union `other` into the live grants.
    pub fn merge_in(&self, other: PermissionGrants) {
        if let Ok(mut g) = self.inner.write() {
            g.merge_with(other);
        }
    }
}

/// Glob match for a single host pattern (case-insensitive):
/// - exact host: `api.example.com` matches only that.
/// - `*.example.com` / `.example.com`: any subdomain **and** the bare apex.
/// - a bare suffix (`example.com`) matches only itself (no substring match, so
///   `evil-example.com` never slips past `example.com`).
#[must_use]
pub fn host_matches_glob(host: &str, pattern: &str) -> bool {
    let h = host.to_ascii_lowercase();
    let p = pattern.to_ascii_lowercase();
    if h == p {
        return true;
    }
    if let Some(suffix) = p.strip_prefix("*.") {
        return h.ends_with(&format!(".{suffix}")) || h == suffix;
    }
    if let Some(suffix) = p.strip_prefix('.') {
        return h.ends_with(&format!(".{suffix}")) || h == suffix;
    }
    false
}

#[cfg(test)]
#[allow(clippy::unwrap_used, clippy::expect_used)]
mod tests {
    use super::*;
    use tempfile::TempDir;

    #[test]
    fn new_pins_schema_version_one() {
        assert_eq!(PermissionGrants::default().schema_version, 0);
        assert_eq!(PermissionGrants::new().schema_version, 1);
    }

    #[test]
    fn host_exact_and_wildcard() {
        let mut g = PermissionGrants::new();
        g.add(GrantQuery::Network("api.example.com".into()));
        assert!(g.matches_host("api.example.com"));
        assert!(g.matches_host("API.EXAMPLE.COM"));
        assert!(!g.matches_host("other.example.com"));

        let mut w = PermissionGrants::new();
        w.add(GrantQuery::Network("*.example.com".into()));
        assert!(w.matches_host("api.example.com"));
        assert!(w.matches_host("example.com")); // bare apex
        assert!(!w.matches_host("evil-example.com"));
    }

    #[test]
    fn bare_host_requires_exact_match() {
        let mut g = PermissionGrants::new();
        g.add(GrantQuery::Network("example.com".into()));
        assert!(g.matches_host("example.com"));
        assert!(!g.matches_host("api.example.com"));
        assert!(!g.matches_host("evil-example.com"));
    }

    #[test]
    fn tool_exact_only() {
        let mut g = PermissionGrants::new();
        g.add(GrantQuery::Tool("web_search".into()));
        assert!(g.matches_tool("web_search"));
        assert!(!g.matches_tool("web_search_v2"));
    }

    #[test]
    fn bash_prefix_with_trailing_space_guard() {
        let mut g = PermissionGrants::new();
        g.add(GrantQuery::Bash("cargo ".into()));
        assert!(g.matches_bash("cargo test"));
        assert!(g.matches_bash("CARGO BUILD"));
        assert!(!g.matches_bash("cargonaut"));
    }

    #[test]
    fn contains_matches_add() {
        let mut g = PermissionGrants::new();
        let q = GrantQuery::Bash("npm ".into());
        assert!(!g.contains(&q));
        g.add(q.clone());
        assert!(g.contains(&q));
    }

    #[test]
    fn merge_unions() {
        let mut a = PermissionGrants::new();
        a.add(GrantQuery::Network("a.example.com".into()));
        let mut b = PermissionGrants::new();
        b.add(GrantQuery::Tool("t".into()));
        b.add(GrantQuery::Bash("pnpm ".into()));
        a.merge_with(b);
        assert!(a.matches_host("a.example.com"));
        assert!(a.matches_tool("t"));
        assert!(a.matches_bash("pnpm i"));
    }

    #[test]
    fn save_load_round_trip() {
        let tmp = TempDir::new().unwrap();
        let path = tmp.path().join("wonk-allow.toml");
        let mut g = PermissionGrants::new();
        g.add(GrantQuery::Network("*.openai.com".into()));
        g.add(GrantQuery::Tool("web_search".into()));
        g.add(GrantQuery::Bash("cargo ".into()));
        g.save_to_path(&path).unwrap();
        assert_eq!(PermissionGrants::load_from_path(&path).unwrap(), g);
    }

    #[test]
    fn load_missing_is_empty_not_error() {
        let tmp = TempDir::new().unwrap();
        let g = PermissionGrants::load_from_path(&tmp.path().join("nope.toml")).unwrap();
        assert_eq!(g.schema_version, 1);
        assert!(g.network.allow_hosts.is_empty());
    }

    #[test]
    fn load_malformed_surfaces_error() {
        let tmp = TempDir::new().unwrap();
        let path = tmp.path().join("wonk-allow.toml");
        std::fs::write(&path, "this is [not valid = toml").unwrap();
        let err = PermissionGrants::load_from_path(&path).unwrap_err();
        assert!(err.to_string().contains("malformed wonk-allow.toml"), "got: {err}");
    }

    #[test]
    fn save_is_atomic_and_creates_dirs() {
        let tmp = TempDir::new().unwrap();
        let path = tmp.path().join("nested").join("dir").join("wonk-allow.toml");
        let mut g = PermissionGrants::new();
        g.add(GrantQuery::Network("a.example.com".into()));
        g.save_to_path(&path).unwrap();
        assert!(path.exists());
        assert!(!path.with_extension("toml.tmp").exists(), "tempfile must be renamed away");
    }

    #[test]
    fn append_grant_creates_then_extends_idempotently() {
        let tmp = TempDir::new().unwrap();
        let path = tmp.path().join("wonk-allow.toml");
        append_grant(&path, GrantQuery::Bash("npm ".into())).unwrap();
        append_grant(&path, GrantQuery::Bash("npm ".into())).unwrap(); // dup
        append_grant(&path, GrantQuery::Network("api.example.com".into())).unwrap();
        let g = PermissionGrants::load_from_path(&path).unwrap();
        assert_eq!(g.bash.allow_patterns.len(), 1);
        assert!(g.matches_bash("npm install left-pad"));
        assert!(g.matches_host("api.example.com"));
    }

    #[test]
    fn load_layered_project_wins_schema_but_unions_entries() {
        let tmp = TempDir::new().unwrap();
        let user = tmp.path().join("user.toml");
        let project = tmp.path().join("project.toml");
        let mut u = PermissionGrants::new();
        u.add(GrantQuery::Bash("cargo ".into()));
        u.save_to_path(&user).unwrap();
        let mut p = PermissionGrants::new();
        p.add(GrantQuery::Bash("pnpm ".into()));
        p.add(GrantQuery::Tool("web_search".into()));
        p.save_to_path(&project).unwrap();

        let merged = PermissionGrants::load_layered(Some(&user), Some(&project)).unwrap();
        assert!(merged.matches_bash("cargo test"), "user grant present");
        assert!(merged.matches_bash("pnpm i"), "project grant present");
        assert!(merged.matches_tool("web_search"));
    }

    #[test]
    fn load_layered_missing_files_yield_empty() {
        let tmp = TempDir::new().unwrap();
        let merged = PermissionGrants::load_layered(Some(&tmp.path().join("u.toml")), Some(&tmp.path().join("p.toml"))).unwrap();
        assert!(merged.network.allow_hosts.is_empty());
        assert!(merged.bash.allow_patterns.is_empty());
    }

    #[test]
    fn shared_snapshot_is_isolated_and_merge_visible() {
        let shared = SharedGrants::new(PermissionGrants::new());
        let mut more = PermissionGrants::new();
        more.add(GrantQuery::Network("b.example.com".into()));
        shared.merge_in(more);
        assert!(shared.snapshot().matches_host("b.example.com"));
        // Mutating a snapshot does not touch the store.
        let mut snap = shared.snapshot();
        snap.add(GrantQuery::Network("c.example.com".into()));
        assert!(!shared.snapshot().matches_host("c.example.com"));
    }

    #[test]
    fn path_helpers() {
        if let Some(p) = user_grants_path() {
            let s = p.to_string_lossy();
            assert!(s.ends_with(".smooth/wonk-allow.toml") || s.ends_with(".smooth\\wonk-allow.toml"));
        }
        assert_eq!(project_grants_path(Path::new("/tmp/x")), PathBuf::from("/tmp/x/.smooth/wonk-allow.toml"));
    }
}
