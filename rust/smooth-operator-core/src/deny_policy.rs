//! Consumer-supplied **deny policy** (pearl th-deny-policy) — the deny-side
//! counterpart to [`permission_grants`](crate::permission_grants).
//!
//! The engine ships hardcoded circuit-breakers (`rm -rf /`, `curl | sh`,
//! credential paths, dangerous domains — see [`crate::permission`]) and an
//! allow-only grant store that can *upgrade* an `Ask`. Neither can express a
//! consumer's own "never do this" rules: "never touch the prod AWS profile",
//! "the DB writer endpoint is off-limits, reads go to the replica", "no writes
//! under `/prod`". This module adds that missing tier.
//!
//! It is **purely additive**: a [`PermissionHook`](crate::permission::PermissionHook)
//! with no deny policy attached behaves byte-for-byte as before. When a policy
//! *is* attached it is evaluated **first**, and a match is a hard deny of the
//! same tier as the built-in circuit-breakers — no stored grant waives it, and
//! [`AutoMode::Bypass`](crate::permission::AutoMode::Bypass) /
//! [`AutoMode::AcceptEdits`](crate::permission::AutoMode::AcceptEdits) cannot
//! downgrade it.
//!
//! # Two tiers
//!
//! 1. **Declarative** ([`DenyRules`]) — TOML, mirroring `permission_grants`'
//!    section style. Four sections, each a deny list:
//!
//!    ```toml
//!    schema_version = 1
//!
//!    [tools]
//!    deny = ["vendor.dangerous_tool", "*.delete_prod"]
//!
//!    [bash]
//!    deny_patterns = ["aws * --profile prod", "kubectl * --context prod"]
//!
//!    [network]
//!    deny_hosts = ["*.prod.internal", "prod-*.rds.amazonaws.com"]
//!
//!    [paths]
//!    deny = ["/prod/**", "**/secrets/**"]
//!    ```
//!
//!    - `tools.deny` — glob on the (possibly dotted) tool name.
//!    - `bash.deny_patterns` — a command **prefix** (`"aws "` — the trailing
//!      space is the `awscli` word-boundary guard, exactly like
//!      `permission_grants`' bash prefixes) or a `*`-glob (`"aws * --profile
//!      prod"`). Applied per-subcommand (compound-aware) after stripping leading
//!      `sudo` / wrappers, so `sudo aws … --profile prod` and `x && aws … --profile
//!      prod` are both caught.
//!    - `network.deny_hosts` — a host suffix (`prod.internal` matches it and any
//!      subdomain, via [`domain_matches_suffix_list`]), a `*.suffix` glob, or a
//!      mid-string glob (`prod-*.rds.amazonaws.com`).
//!    - `paths.deny` — a path glob (`*`/`**` both match any run, including `/`)
//!      checked against the `path`/`file`/`dir` arg of Write and Read tools.
//!
//! 2. **Predicate** ([`DenyPredicate`]) — a boxed trait the consumer supplies
//!    for semantic checks the engine cannot parse from strings: "is this AWS
//!    call the *prod account*?", "is this DB connection the *writer* endpoint?".
//!    `Some(reason)` → deny.
//!
//! Both run on every gated tool call; declarative first, then predicates. The
//! first match wins.

use std::collections::BTreeSet;
use std::sync::Arc;

use serde::{Deserialize, Serialize};

use crate::permission::{domain_matches_suffix_list, extract_hosts, host_from_token, split_compound, strip_wrappers_and_sudo, tool_category, Category};
use crate::tool::ToolCall;

// ---------------------------------------------------------------------------
// Declarative rules (TOML)
// ---------------------------------------------------------------------------

/// The declarative half of a [`DenyPolicy`]: four deny lists parsed from TOML.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct DenyRules {
    /// Reserved for forward-compatible migrations. Written as 1 by [`Self::new`].
    pub schema_version: u32,
    #[serde(skip_serializing_if = "ToolsDeny::is_empty", default)]
    pub tools: ToolsDeny,
    #[serde(skip_serializing_if = "BashDeny::is_empty", default)]
    pub bash: BashDeny,
    #[serde(skip_serializing_if = "NetworkDeny::is_empty", default)]
    pub network: NetworkDeny,
    #[serde(skip_serializing_if = "PathsDeny::is_empty", default)]
    pub paths: PathsDeny,
}

/// `[tools]` — deny by tool name / glob.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct ToolsDeny {
    pub deny: BTreeSet<String>,
}

impl ToolsDeny {
    fn is_empty(&self) -> bool {
        self.deny.is_empty()
    }
}

/// `[bash]` — deny bash command prefixes / globs.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct BashDeny {
    pub deny_patterns: BTreeSet<String>,
}

impl BashDeny {
    fn is_empty(&self) -> bool {
        self.deny_patterns.is_empty()
    }
}

/// `[network]` — deny host suffixes / globs.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct NetworkDeny {
    pub deny_hosts: BTreeSet<String>,
}

impl NetworkDeny {
    fn is_empty(&self) -> bool {
        self.deny_hosts.is_empty()
    }
}

/// `[paths]` — deny file paths / globs (Write + Read tools).
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default)]
pub struct PathsDeny {
    pub deny: BTreeSet<String>,
}

impl PathsDeny {
    fn is_empty(&self) -> bool {
        self.deny.is_empty()
    }
}

impl DenyRules {
    /// New empty rules pinned at the current schema version.
    #[must_use]
    pub fn new() -> Self {
        Self {
            schema_version: 1,
            ..Self::default()
        }
    }

    /// No rules in any section (used for the additive no-op fast path).
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.tools.is_empty() && self.bash.is_empty() && self.network.is_empty() && self.paths.is_empty()
    }

    /// Parse from a TOML string. Missing sections default to empty.
    ///
    /// # Errors
    /// Returns the TOML parse error if the input isn't valid.
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

    /// The first declarative rule this call matches, formatted as a deny reason.
    fn deny_reason(&self, call: &ToolCall) -> Option<String> {
        // `[tools]` applies to ANY tool, whatever its category.
        if let Some(pat) = self.tools.deny.iter().find(|p| glob_match(p, &call.name)) {
            return Some(format!("denied by policy (tools): {pat}"));
        }
        let args = &call.arguments;
        match tool_category(&call.name) {
            Category::Bash => {
                let cmd = args.get("cmd").or_else(|| args.get("command")).and_then(|v| v.as_str()).unwrap_or("").trim();
                if cmd.is_empty() {
                    return None;
                }
                if let Some(pat) = self.bash_denied(cmd) {
                    return Some(format!("denied by policy (bash): {pat}"));
                }
                // A denied host referenced by the command line is also blocked.
                for sub in split_compound(cmd) {
                    for host in extract_hosts(&sub) {
                        if let Some(pat) = self.host_denied(&host) {
                            return Some(format!("denied by policy (network): {pat}"));
                        }
                    }
                }
                None
            }
            Category::Network => {
                let raw = args.get("url").or_else(|| args.get("host")).and_then(|v| v.as_str()).unwrap_or("");
                let host = host_from_token(raw).unwrap_or_else(|| raw.to_string());
                if host.is_empty() {
                    return None;
                }
                self.host_denied(&host).map(|pat| format!("denied by policy (network): {pat}"))
            }
            Category::Write | Category::Safe => {
                for key in ["path", "file", "dir", "directory"] {
                    if let Some(v) = args.get(key).and_then(|v| v.as_str()) {
                        if let Some(pat) = self.paths.deny.iter().find(|p| glob_match(p, v)) {
                            return Some(format!("denied by policy (paths): {pat}"));
                        }
                    }
                }
                None
            }
            Category::Unknown => None,
        }
    }

    /// First `[bash]` pattern that matches any (wrapper/sudo-stripped) subcommand.
    fn bash_denied(&self, cmd: &str) -> Option<&String> {
        let subs: Vec<String> = split_compound(cmd).iter().map(|s| strip_wrappers_and_sudo(s).to_ascii_lowercase()).collect();
        self.bash.deny_patterns.iter().find(|pat| {
            // A plain prefix (`"aws "`) gets an implicit trailing `*`; a pattern
            // with an explicit `*` (`"aws * --profile prod"`) also matches any
            // trailing text so extra flags don't slip a call past the rule.
            let lower = pat.to_ascii_lowercase();
            let anchored = if lower.ends_with('*') { lower } else { format!("{lower}*") };
            subs.iter().any(|sub| glob_match(&anchored, sub))
        })
    }

    /// First `[network]` pattern that matches `host` (case-insensitive).
    fn host_denied(&self, host: &str) -> Option<&String> {
        let h = host.to_ascii_lowercase();
        self.network.deny_hosts.iter().find(|pat| host_pattern_matches(pat, &h))
    }
}

/// Match a single host deny pattern against an already-lowercased host.
/// - no `*` → subdomain-aware suffix match (`prod.internal` ⇒ `api.prod.internal`).
/// - `*.suffix` → apex + subdomains of `suffix`.
/// - mid-string `*` (`prod-*.rds.amazonaws.com`) → anchored glob.
fn host_pattern_matches(pattern: &str, host_lower: &str) -> bool {
    let p = pattern.to_ascii_lowercase();
    if !p.contains('*') {
        return domain_matches_suffix_list(host_lower, &[&p]);
    }
    if let Some(bare) = p.strip_prefix("*.") {
        if domain_matches_suffix_list(host_lower, &[bare]) {
            return true;
        }
    }
    glob_match(&p, host_lower)
}

/// Minimal both-ends-anchored glob: `*` (and any run of `*`, so `**` too)
/// matches any sequence of characters, including `/`. No `?`, no char classes —
/// deny globs don't need them, and a tiny matcher stays auditable for a
/// security-critical path.
pub(crate) fn glob_match(pattern: &str, text: &str) -> bool {
    let parts: Vec<&str> = pattern.split('*').collect();
    if parts.len() == 1 {
        return pattern == text; // no wildcard → exact match
    }
    // First literal segment is anchored at the start.
    let first = parts[0];
    if !text.starts_with(first) {
        return false;
    }
    let mut pos = first.len();
    let last_idx = parts.len() - 1;
    for (i, part) in parts.iter().enumerate().skip(1) {
        if part.is_empty() {
            continue; // consecutive/trailing `*`
        }
        if i == last_idx {
            // Last literal segment must sit at the very end, and must not
            // overlap the region already consumed by earlier segments.
            let Some(end_start) = text.len().checked_sub(part.len()) else {
                return false;
            };
            return end_start >= pos && text[pos..].ends_with(part);
        }
        match text[pos..].find(part) {
            Some(idx) => pos += idx + part.len(),
            None => return false,
        }
    }
    // Pattern ended with `*` (last part empty): the trailing run matches anything.
    true
}

// ---------------------------------------------------------------------------
// Predicate tier
// ---------------------------------------------------------------------------

/// The reason a [`DenyPredicate`] blocks a call. A thin newtype over `String`
/// so the predicate contract is explicit and the type can grow structured
/// fields later without breaking the signature.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DenyReason(pub String);

impl DenyReason {
    #[must_use]
    pub fn new(reason: impl Into<String>) -> Self {
        Self(reason.into())
    }
}

impl From<&str> for DenyReason {
    fn from(s: &str) -> Self {
        Self(s.to_string())
    }
}

impl From<String> for DenyReason {
    fn from(s: String) -> Self {
        Self(s)
    }
}

/// A consumer-supplied semantic deny check. Runs on every gated tool call; a
/// `Some(reason)` is a hard deny (circuit-breaker tier). Use it for the checks
/// the declarative rules can't express from strings alone — resolving an AWS
/// call to its account, a DB URL to writer-vs-replica, etc.
pub trait DenyPredicate: Send + Sync {
    /// Return `Some(reason)` to deny `call`, `None` to let it fall through to the
    /// rest of the permission engine.
    fn evaluate(&self, call: &ToolCall) -> Option<DenyReason>;
}

// ---------------------------------------------------------------------------
// The assembled policy
// ---------------------------------------------------------------------------

/// Consumer-supplied deny policy: declarative rules + predicate checks. Attach
/// to the gate via
/// [`PermissionHook::with_deny_policy`](crate::permission::PermissionHook::with_deny_policy)
/// or [`Agent::with_deny_policy`](crate::agent::Agent::with_deny_policy).
///
/// Cheaply cloned (predicates are `Arc`). An empty policy is a no-op.
#[derive(Clone, Default)]
pub struct DenyPolicy {
    declarative: DenyRules,
    predicates: Vec<Arc<dyn DenyPredicate>>,
}

impl std::fmt::Debug for DenyPolicy {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("DenyPolicy")
            .field("declarative", &self.declarative)
            .field("predicates", &self.predicates.len())
            .finish()
    }
}

impl DenyPolicy {
    /// An empty policy — denies nothing (the additive no-op default).
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Build the declarative half from a TOML string. Predicates are added
    /// separately via [`with_predicate`](Self::with_predicate).
    ///
    /// # Errors
    /// Propagates the TOML parse error.
    pub fn from_toml(toml_text: &str) -> anyhow::Result<Self> {
        Ok(Self {
            declarative: DenyRules::parse(toml_text)?,
            predicates: Vec::new(),
        })
    }

    /// Replace the declarative rules.
    #[must_use]
    pub fn with_declarative(mut self, rules: DenyRules) -> Self {
        self.declarative = rules;
        self
    }

    /// Add a consumer predicate. Chainable.
    #[must_use]
    pub fn with_predicate(mut self, predicate: Arc<dyn DenyPredicate>) -> Self {
        self.predicates.push(predicate);
        self
    }

    /// True when there are no rules and no predicates — nothing to deny.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.declarative.is_empty() && self.predicates.is_empty()
    }

    /// The deny reason for `call`, or `None` to let it fall through to the rest
    /// of the permission engine. Declarative rules are checked first, then
    /// predicates; the first match wins.
    #[must_use]
    pub fn evaluate(&self, call: &ToolCall) -> Option<String> {
        if let Some(reason) = self.declarative.deny_reason(call) {
            return Some(reason);
        }
        for predicate in &self.predicates {
            if let Some(reason) = predicate.evaluate(call) {
                return Some(format!("denied by policy (predicate): {}", reason.0));
            }
        }
        None
    }
}

#[cfg(test)]
#[allow(clippy::unwrap_used)]
mod tests {
    use super::*;
    use serde_json::json;

    fn call(name: &str, args: serde_json::Value) -> ToolCall {
        ToolCall {
            id: "c1".into(),
            name: name.into(),
            arguments: args,
        }
    }

    fn bash_call(cmd: &str) -> ToolCall {
        call("bash", json!({ "cmd": cmd }))
    }

    // ── glob matcher ───────────────────────────────────────────────

    #[test]
    fn glob_exact_and_wildcards() {
        assert!(glob_match("exact", "exact"));
        assert!(!glob_match("exact", "exacts"));
        assert!(glob_match("vendor.*", "vendor.delete"));
        assert!(!glob_match("vendor.*", "other.delete"));
        assert!(glob_match("*.delete", "vendor.delete"));
        assert!(!glob_match("*.delete", "vendor.deleted"));
        assert!(glob_match("a*c", "abc"));
        assert!(glob_match("a*c", "ac"));
        assert!(!glob_match("a*c", "ab"));
        assert!(glob_match("/prod/**", "/prod/secrets/db.txt"));
        assert!(!glob_match("/prod/**", "/staging/x"));
        assert!(glob_match("**/secrets/**", "/a/b/secrets/c/d"));
        assert!(!glob_match("**/secrets/**", "/a/b/c"));
    }

    // ── declarative: tools ─────────────────────────────────────────

    #[test]
    fn tools_section_denies_match_allows_nonmatch() {
        let policy = DenyPolicy::from_toml(
            r#"
            [tools]
            deny = ["vendor.dangerous_tool", "*.delete_prod"]
        "#,
        )
        .unwrap();
        assert!(policy.evaluate(&call("vendor.dangerous_tool", json!({}))).is_some());
        assert!(policy.evaluate(&call("svc.delete_prod", json!({}))).is_some());
        // Non-match falls through.
        assert!(policy.evaluate(&call("vendor.safe_tool", json!({}))).is_none());
    }

    // ── declarative: bash ──────────────────────────────────────────

    #[test]
    fn bash_section_denies_match_allows_nonmatch() {
        let policy = DenyPolicy::from_toml(
            r#"
            [bash]
            deny_patterns = ["aws * --profile prod", "terraform apply"]
        "#,
        )
        .unwrap();
        assert!(policy.evaluate(&bash_call("aws s3 ls --profile prod")).is_some());
        assert!(policy.evaluate(&bash_call("terraform apply -auto-approve")).is_some());
        // A non-prod profile is fine.
        assert!(policy.evaluate(&bash_call("aws s3 ls --profile dev")).is_none());
        // Word boundary: the trailing space in "terraform apply" is not required
        // here (glob adds trailing *), but a different binary must not match.
        assert!(policy.evaluate(&bash_call("aws s3 ls")).is_none());
    }

    #[test]
    fn bash_prefix_word_boundary() {
        let policy = DenyPolicy::from_toml(
            r#"
            [bash]
            deny_patterns = ["aws "]
        "#,
        )
        .unwrap();
        assert!(policy.evaluate(&bash_call("aws s3 ls")).is_some());
        // The trailing space guards against `awslocal`.
        assert!(policy.evaluate(&bash_call("awslocal s3 ls")).is_none());
    }

    #[test]
    fn bash_deny_survives_sudo_and_compound_and_extra_flags() {
        let policy = DenyPolicy::from_toml(
            r#"
            [bash]
            deny_patterns = ["aws * --profile prod"]
        "#,
        )
        .unwrap();
        // Adversarial: sudo prefix.
        assert!(policy.evaluate(&bash_call("sudo aws s3 rm s3://b --profile prod")).is_some());
        // Adversarial: hidden in a compound after a safe command.
        assert!(policy.evaluate(&bash_call("ls && aws s3 ls --profile prod")).is_some());
        // Adversarial: extra trailing flags after the matched suffix.
        assert!(policy.evaluate(&bash_call("aws s3 ls --profile prod --region us-east-1")).is_some());
        // Wrapper strip.
        assert!(policy.evaluate(&bash_call("timeout 5 aws s3 ls --profile prod")).is_some());
    }

    // ── declarative: network ───────────────────────────────────────

    #[test]
    fn network_section_denies_suffix_and_glob() {
        let policy = DenyPolicy::from_toml(
            r#"
            [network]
            deny_hosts = ["*.prod.internal", "prod-*.rds.amazonaws.com", "secrets.example.com"]
        "#,
        )
        .unwrap();
        // Direct network tool.
        assert!(policy.evaluate(&call("web_fetch", json!({"url": "https://api.prod.internal/x"}))).is_some());
        // Apex of the *.suffix pattern.
        assert!(policy.evaluate(&call("web_fetch", json!({"url": "https://prod.internal/"}))).is_some());
        // Mid-string glob.
        assert!(policy
            .evaluate(&call("web_fetch", json!({"url": "https://prod-db1.rds.amazonaws.com"})))
            .is_some());
        // Bare suffix is subdomain-aware.
        assert!(policy.evaluate(&call("web_fetch", json!({"host": "api.secrets.example.com"}))).is_some());
        // Non-match.
        assert!(policy.evaluate(&call("web_fetch", json!({"url": "https://staging.internal/x"}))).is_none());
        // Also catches a curl in bash.
        assert!(policy.evaluate(&bash_call("curl https://api.prod.internal/health")).is_some());
    }

    // ── declarative: paths ─────────────────────────────────────────

    #[test]
    fn paths_section_denies_write_and_read() {
        let policy = DenyPolicy::from_toml(
            r#"
            [paths]
            deny = ["/prod/**", "**/secrets/**"]
        "#,
        )
        .unwrap();
        assert!(policy.evaluate(&call("file_write", json!({"path": "/prod/config.yaml"}))).is_some());
        assert!(policy.evaluate(&call("read_file", json!({"path": "/app/secrets/db.env"}))).is_some());
        assert!(policy.evaluate(&call("list_dir", json!({"dir": "/prod/data"}))).is_some());
        // Non-match.
        assert!(policy.evaluate(&call("file_write", json!({"path": "/app/src/main.rs"}))).is_none());
    }

    // ── predicate tier ─────────────────────────────────────────────

    struct ProdAccountPredicate;
    impl DenyPredicate for ProdAccountPredicate {
        fn evaluate(&self, call: &ToolCall) -> Option<DenyReason> {
            let cmd = call.arguments.get("cmd").and_then(|v| v.as_str()).unwrap_or("");
            // Semantic: an engine can't parse "account 999" as prod; the consumer can.
            if cmd.contains("999999999999") {
                Some(DenyReason::new("resolved to the prod AWS account"))
            } else {
                None
            }
        }
    }

    #[test]
    fn predicate_some_denies_none_falls_through() {
        let policy = DenyPolicy::new().with_predicate(Arc::new(ProdAccountPredicate));
        let denied = policy.evaluate(&bash_call("aws s3 ls --profile acct-999999999999"));
        assert!(denied.unwrap().contains("prod AWS account"));
        // A different account falls through.
        assert!(policy.evaluate(&bash_call("aws s3 ls --profile acct-111")).is_none());
    }

    // ── empty policy = no-op ───────────────────────────────────────

    #[test]
    fn empty_policy_denies_nothing() {
        let policy = DenyPolicy::new();
        assert!(policy.is_empty());
        assert!(policy.evaluate(&bash_call("rm -rf /prod")).is_none());
        assert!(policy.evaluate(&call("file_write", json!({"path": "/prod/x"}))).is_none());
        assert!(policy.evaluate(&call("vendor.anything", json!({}))).is_none());
    }

    // ── TOML round-trip ────────────────────────────────────────────

    #[test]
    fn toml_round_trip() {
        let mut rules = DenyRules::new();
        rules.tools.deny.insert("vendor.dangerous_tool".into());
        rules.bash.deny_patterns.insert("aws * --profile prod".into());
        rules.network.deny_hosts.insert("*.prod.internal".into());
        rules.paths.deny.insert("/prod/**".into());
        let text = rules.to_toml_string().unwrap();
        assert_eq!(DenyRules::parse(&text).unwrap(), rules);
    }

    #[test]
    fn empty_rules_parse_and_are_empty() {
        assert!(DenyRules::parse("").unwrap().is_empty());
        assert!(DenyRules::parse("schema_version = 1").unwrap().is_empty());
    }

    // ── precedence: declarative before predicate ───────────────────

    struct AlwaysDeny;
    impl DenyPredicate for AlwaysDeny {
        fn evaluate(&self, _call: &ToolCall) -> Option<DenyReason> {
            Some(DenyReason::new("predicate always denies"))
        }
    }

    #[test]
    fn declarative_reason_wins_over_predicate() {
        let policy = DenyPolicy::from_toml(
            r#"
            [tools]
            deny = ["vendor.tool"]
        "#,
        )
        .unwrap()
        .with_predicate(Arc::new(AlwaysDeny));
        // Declarative match surfaces first.
        assert!(policy.evaluate(&call("vendor.tool", json!({}))).unwrap().contains("(tools)"));
        // No declarative match → predicate.
        assert!(policy.evaluate(&call("other.tool", json!({}))).unwrap().contains("(predicate)"));
    }
}
