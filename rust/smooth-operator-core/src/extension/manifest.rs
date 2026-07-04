//! Extension manifests — `extension.toml` discovery, merge, and `${env:VAR}`
//! expansion.
//!
//! Mirrors the MCP config pattern in `smooth-operative/src/mcp.rs`:
//!
//! - An extension lives in a directory holding an `extension.toml`.
//! - Global extensions: `~/.smooth/extensions/<name>/extension.toml`.
//! - Project extensions: `<workspace>/.smooth/extensions/<name>/extension.toml`.
//! - On a name collision the **project entry wins** (byte-for-byte the
//!   mcp.toml / plugin.toml merge rule).
//! - `[run] env` values support `${env:VAR}` expansion so secrets stay out of
//!   the manifest.
//! - A single malformed manifest is tolerated: it is collected as a failure and
//!   the rest still load.

use std::collections::HashMap;
use std::path::{Path, PathBuf};

use serde::{Deserialize, Serialize};

/// Where a manifest was discovered. Project extensions only load in trusted
/// workspaces; the host uses this to apply that policy.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Scope {
    Global,
    Project,
}

impl Scope {
    #[must_use]
    pub fn as_str(self) -> &'static str {
        match self {
            Scope::Global => "global",
            Scope::Project => "project",
        }
    }
}

/// How to launch the extension subprocess.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq)]
pub struct RunSpec {
    /// Executable to spawn (e.g. `node`, `python3`, an absolute path).
    pub command: String,
    #[serde(default)]
    pub args: Vec<String>,
    /// Extra env vars; values may reference `${env:VAR}`.
    #[serde(default)]
    pub env: HashMap<String, String>,
    /// Optional pinned SHA-256 (lowercase hex) of the resolved `command`
    /// binary. When set, the host refuses to spawn the extension unless the
    /// on-disk binary hashes to exactly this value — integrity verification is
    /// a SECOND gate after the load allow-list. When unset, the host records
    /// the observed hash (TOFU) so a consumer can pin it.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub sha256: Option<String>,
}

/// Capability declarations. The `events` list doubles as the host's dispatch
/// filter — an extension only receives events it names here.
#[derive(Debug, Clone, Default, Deserialize, Serialize, PartialEq)]
pub struct Capabilities {
    #[serde(default)]
    pub events: Vec<String>,
    #[serde(default)]
    pub tools: bool,
    #[serde(default)]
    pub commands: bool,
    #[serde(default)]
    pub ui: bool,
    #[serde(default)]
    pub exec: bool,
    #[serde(default)]
    pub kv: bool,
    #[serde(default)]
    pub bus: bool,
    #[serde(default)]
    pub session: bool,
}

/// Resource directories the extension contributes (skills, prompts, themes).
#[derive(Debug, Clone, Default, Deserialize, Serialize, PartialEq)]
pub struct Resources {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub skills: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub prompts: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub themes: Option<String>,
}

/// A parsed `extension.toml`.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq)]
pub struct ExtensionManifest {
    pub name: String,
    pub version: String,
    /// Highest SEP protocol version the extension declares. Defaults to 1.
    #[serde(default = "default_protocol")]
    pub protocol: u32,
    pub run: RunSpec,
    #[serde(default)]
    pub capabilities: Capabilities,
    #[serde(default)]
    pub resources: Resources,
    /// Per-extension hook timeout override, in milliseconds.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub hook_timeout_ms: Option<u64>,
    /// Optional: skip this extension without deleting its manifest.
    #[serde(default)]
    pub disabled: bool,
}

fn default_protocol() -> u32 {
    1
}

impl ExtensionManifest {
    /// Parse a manifest from TOML text.
    ///
    /// # Errors
    /// Returns an error if the TOML is malformed or missing required fields.
    pub fn parse(toml_text: &str) -> anyhow::Result<Self> {
        toml::from_str(toml_text).map_err(|e| anyhow::anyhow!("parse extension.toml: {e}"))
    }

    /// Load a manifest from `<dir>/extension.toml`.
    ///
    /// # Errors
    /// Returns an error if the file is missing or malformed.
    pub fn load_dir(dir: &Path) -> anyhow::Result<Self> {
        let path = dir.join("extension.toml");
        let text = std::fs::read_to_string(&path).map_err(|e| anyhow::anyhow!("read {}: {e}", path.display()))?;
        Self::parse(&text)
    }

    /// Return the `[run] env` map with `${env:VAR}` references expanded against
    /// the host's current environment. Unset variables expand to empty strings.
    #[must_use]
    pub fn resolved_env(&self) -> HashMap<String, String> {
        self.run.env.iter().map(|(k, v)| (k.clone(), expand_env(v))).collect()
    }
}

/// A discovered extension: its manifest plus the directory it was found in
/// (relative resources and `args` resolve against this root) and its scope.
#[derive(Debug, Clone)]
pub struct DiscoveredExtension {
    pub manifest: ExtensionManifest,
    pub root: PathBuf,
    pub scope: Scope,
}

/// Default global extensions directory: `$SMOOTH_HOME/extensions` if set, else
/// `~/.smooth/extensions`.
#[must_use]
pub fn default_global_dir() -> Option<PathBuf> {
    if let Ok(home) = std::env::var("SMOOTH_HOME") {
        return Some(PathBuf::from(home).join("extensions"));
    }
    dirs_next::home_dir().map(|h| h.join(".smooth").join("extensions"))
}

/// The project extensions directory for a workspace root.
#[must_use]
pub fn project_dir(workspace_root: &Path) -> PathBuf {
    workspace_root.join(".smooth").join("extensions")
}

/// Discover every extension under `global_dir` and `project_dir`, merging by
/// name with **project winning**. Either directory may be `None` or missing
/// (treated as empty). Returns the chosen extensions plus a list of
/// `(name_or_dir, error)` for manifests that failed to parse — a single bad
/// manifest never aborts discovery.
#[must_use]
pub fn discover(global_dir: Option<&Path>, project_dir: Option<&Path>) -> (Vec<DiscoveredExtension>, Vec<(String, String)>) {
    let mut failures = Vec::new();
    let mut by_name: HashMap<String, DiscoveredExtension> = HashMap::new();

    // Global first, then project, so project overwrites on name collision.
    for (dir, scope) in [(global_dir, Scope::Global), (project_dir, Scope::Project)] {
        let Some(dir) = dir else { continue };
        for found in scan_dir(dir, scope, &mut failures) {
            if scope == Scope::Project {
                if let Some(prev) = by_name.get(&found.manifest.name) {
                    if prev.scope == Scope::Global {
                        tracing::info!(name = %found.manifest.name, "extension: project manifest overrides global");
                    }
                }
            }
            by_name.insert(found.manifest.name.clone(), found);
        }
    }

    let mut chosen: Vec<DiscoveredExtension> = by_name.into_values().collect();
    // Stable order so load-order-dependent hook chaining is deterministic.
    chosen.sort_by_key(|e| e.manifest.name.clone());
    (chosen, failures)
}

/// Scan a single extensions directory: each immediate subdirectory holding an
/// `extension.toml` is one extension.
fn scan_dir(dir: &Path, scope: Scope, failures: &mut Vec<(String, String)>) -> Vec<DiscoveredExtension> {
    let mut out = Vec::new();
    let entries = match std::fs::read_dir(dir) {
        Ok(e) => e,
        // Missing dir is not an error — just no extensions from this scope.
        Err(_) => return out,
    };
    for entry in entries.flatten() {
        let root = entry.path();
        if !root.is_dir() {
            continue;
        }
        if !root.join("extension.toml").is_file() {
            continue;
        }
        match ExtensionManifest::load_dir(&root) {
            Ok(manifest) => out.push(DiscoveredExtension { manifest, root, scope }),
            Err(e) => failures.push((root.display().to_string(), e.to_string())),
        }
    }
    out
}

/// Expand `${env:VAR}` references using the host's current environment. Unset
/// variables expand to empty strings. Copied from the MCP loader so the two
/// config surfaces behave identically.
fn expand_env(input: &str) -> String {
    let mut out = String::with_capacity(input.len());
    let mut rest = input;
    while let Some(idx) = rest.find("${env:") {
        out.push_str(&rest[..idx]);
        let after = &rest[idx + 6..];
        if let Some(end) = after.find('}') {
            let var = &after[..end];
            out.push_str(&std::env::var(var).unwrap_or_default());
            rest = &after[end + 1..];
        } else {
            out.push_str(&rest[idx..]);
            return out;
        }
    }
    out.push_str(rest);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    const MINIMAL: &str = r#"
name = "echo"
version = "0.1.0"
[run]
command = "node"
args = ["echo.mjs"]
"#;

    #[test]
    fn parses_minimal_manifest_with_defaults() {
        let m = ExtensionManifest::parse(MINIMAL).expect("parse");
        assert_eq!(m.name, "echo");
        assert_eq!(m.protocol, 1); // default
        assert_eq!(m.run.command, "node");
        assert_eq!(m.run.args, vec!["echo.mjs"]);
        assert!(!m.disabled);
        assert!(m.run.sha256.is_none()); // no pin by default → TOFU
        assert!(m.capabilities.events.is_empty());
    }

    #[test]
    fn parses_full_manifest() {
        let text = r#"
name = "gate"
version = "2.0.0"
protocol = 1
hook_timeout_ms = 3000
[run]
command = "python3"
args = ["-m", "gate"]
env = { TOKEN = "${env:GATE_TOKEN}", STATIC = "x" }
sha256 = "abc123"
[capabilities]
events = ["turn_start", "tool_call"]
tools = true
ui = true
[resources]
skills = "skills"
"#;
        let m = ExtensionManifest::parse(text).expect("parse");
        assert_eq!(m.hook_timeout_ms, Some(3000));
        assert_eq!(m.run.sha256.as_deref(), Some("abc123"));
        assert!(m.capabilities.tools && m.capabilities.ui && !m.capabilities.exec);
        assert_eq!(m.capabilities.events, vec!["turn_start", "tool_call"]);
        assert_eq!(m.resources.skills.as_deref(), Some("skills"));
    }

    #[test]
    fn malformed_manifest_errors() {
        assert!(ExtensionManifest::parse("name = 3\n").is_err());
        assert!(ExtensionManifest::parse("not toml : : :").is_err());
    }

    #[test]
    fn resolved_env_expands_env_refs() {
        // Safety: single-threaded test; set + read one var.
        std::env::set_var("SEP_TEST_TOKEN", "secret123");
        let text = r#"
name = "e"
version = "1"
[run]
command = "c"
env = { A = "pre-${env:SEP_TEST_TOKEN}-post", B = "${env:SEP_TEST_UNSET_XYZ}" }
"#;
        let m = ExtensionManifest::parse(text).expect("parse");
        let env = m.resolved_env();
        assert_eq!(env.get("A").unwrap(), "pre-secret123-post");
        assert_eq!(env.get("B").unwrap(), ""); // unset -> empty
        std::env::remove_var("SEP_TEST_TOKEN");
    }

    #[test]
    fn expand_env_handles_unterminated_ref() {
        assert_eq!(expand_env("a${env:FOO"), "a${env:FOO");
        assert_eq!(expand_env("plain"), "plain");
    }

    fn write_ext(dir: &Path, name: &str, body: &str) {
        let ext_dir = dir.join(name);
        std::fs::create_dir_all(&ext_dir).unwrap();
        std::fs::write(ext_dir.join("extension.toml"), body).unwrap();
    }

    #[test]
    fn discover_merges_project_over_global() {
        let tmp = tempfile::tempdir().unwrap();
        let global = tmp.path().join("global");
        let project = tmp.path().join("project");

        write_ext(&global, "echo", "name=\"echo\"\nversion=\"1.0.0\"\n[run]\ncommand=\"g\"\n");
        write_ext(&global, "only_global", "name=\"only_global\"\nversion=\"1\"\n[run]\ncommand=\"g\"\n");
        // Project has an echo that should win, plus its own.
        write_ext(&project, "echo", "name=\"echo\"\nversion=\"2.0.0\"\n[run]\ncommand=\"p\"\n");
        write_ext(&project, "only_project", "name=\"only_project\"\nversion=\"1\"\n[run]\ncommand=\"p\"\n");

        let (found, failures) = discover(Some(&global), Some(&project));
        assert!(failures.is_empty(), "{failures:?}");
        assert_eq!(found.len(), 3);

        let echo = found.iter().find(|e| e.manifest.name == "echo").unwrap();
        assert_eq!(echo.manifest.version, "2.0.0"); // project won
        assert_eq!(echo.scope, Scope::Project);
        assert!(found.iter().any(|e| e.manifest.name == "only_global" && e.scope == Scope::Global));
        assert!(found.iter().any(|e| e.manifest.name == "only_project"));
    }

    #[test]
    fn discover_tolerates_one_broken_manifest() {
        let tmp = tempfile::tempdir().unwrap();
        let global = tmp.path().join("g");
        write_ext(&global, "good", "name=\"good\"\nversion=\"1\"\n[run]\ncommand=\"c\"\n");
        write_ext(&global, "bad", "this is not = = valid toml\n[[[");

        let (found, failures) = discover(Some(&global), None);
        assert_eq!(found.len(), 1);
        assert_eq!(found[0].manifest.name, "good");
        assert_eq!(failures.len(), 1);
        assert!(failures[0].0.contains("bad"));
    }

    #[test]
    fn discover_missing_dirs_is_empty_not_error() {
        let (found, failures) = discover(Some(Path::new("/no/such/global")), Some(Path::new("/no/such/project")));
        assert!(found.is_empty());
        assert!(failures.is_empty());
    }
}
