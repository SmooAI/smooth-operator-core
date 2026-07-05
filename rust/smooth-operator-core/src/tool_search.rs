//! `tool_search` meta-tool — promotes deferred tools on demand
//! (pearl th-cfa1fb).
//!
//! Deferred tools live in [`ToolRegistry::register_deferred`] but
//! their schemas are hidden from the LLM. When the model needs one
//! it calls `tool_search("description …")`; this tool fuzzy-matches
//! the query against the deferred tools' names and descriptions,
//! promotes matches into the eager set, and returns each match's
//! schema as JSON. On the next iteration the agent recomputes its
//! tool list and the LLM can call the matched tool directly.
//!
//! Why this exists: as tool count grows past ~20-30 the LLM's
//! attention budget on the schema list dilutes — every turn pays
//! tokens to read tools it isn't going to use. The opencode/Claude
//! Code pattern keeps a small core eager set and defers the rest
//! behind a search step. This is Pillar D5 of the typed-sniffing-
//! badger plan.

use std::sync::Arc;

use async_trait::async_trait;
use serde_json::{json, Value};

use crate::tool::{Tool, ToolRegistry, ToolSchema};

/// `Arc`-shareable handle to a `ToolRegistry`. The meta-tool needs
/// a way to call `promote()` from within its `execute()` method —
/// since `execute()` takes `&self` and the registry's `promote` is
/// also `&self` (interior mut via `Arc<Mutex<…>>`), wrapping the
/// registry in an `Arc` and storing it in the meta-tool gives us
/// the call site without re-architecting the agent loop.
#[derive(Clone)]
pub struct ToolSearch {
    registry: Arc<ToolRegistry>,
}

impl ToolSearch {
    /// Construct the meta-tool with a handle to the registry whose
    /// deferred tools it will search and promote.
    #[must_use]
    pub fn new(registry: Arc<ToolRegistry>) -> Self {
        Self { registry }
    }
}

#[async_trait]
impl Tool for ToolSearch {
    fn schema(&self) -> ToolSchema {
        ToolSchema {
            name: "tool_search".into(),
            description: "Search for additional tools by keyword. \
                Returns matching tool schemas as JSON; matched tools \
                become available on subsequent turns. Use when you \
                think a tool exists for a specific task but isn't in \
                your current tool list — e.g. tool_search(query=\"git\") \
                or tool_search(query=\"http request\")."
                .into(),
            parameters: json!({
                "type": "object",
                "properties": {
                    "query": {
                        "type": "string",
                        "description": "Keyword to match against deferred tool names and descriptions. Case-insensitive substring match."
                    }
                },
                "required": ["query"]
            }),
        }
    }

    fn is_concurrent_safe(&self) -> bool {
        true
    }

    fn is_read_only(&self) -> bool {
        true
    }

    async fn execute(&self, arguments: Value) -> anyhow::Result<String> {
        let query = arguments
            .get("query")
            .and_then(|v| v.as_str())
            .ok_or_else(|| anyhow::anyhow!("tool_search: missing required `query` parameter"))?;
        let needle = query.trim().to_lowercase();
        if needle.is_empty() {
            return Ok(json!({
                "matched": 0,
                "tools": [],
                "note": "empty query — pass a keyword like \"git\" or \"network\""
            })
            .to_string());
        }

        let summary = self.registry.deferred_summary();
        // ponytail: unbounded case-insensitive substring match, kept as-is. A
        // prompt-injection payload can make the LLM promote a deferred exec tool
        // (e.g. `bash`), but promotion alone is inert: `PermissionHook` (a
        // `ToolHook`) gates the *invocation* in `ToolRegistry::execute` — its
        // `pre_call` runs before the tool is resolved, so a promoted-but-
        // dangerous call is still denied (see `permission_hook_gates_promoted_deferred_tool`).
        // The `MAX_MATCHES` cap bounds a single query's blast radius. No
        // per-tool promote allowlist is warranted while that defense holds.
        let mut matched: Vec<(String, String)> = summary
            .into_iter()
            .filter(|(name, desc)| name.to_lowercase().contains(&needle) || desc.to_lowercase().contains(&needle))
            .collect();

        // Cap matches so a generic query like "tool" doesn't
        // promote the entire deferred set in one shot.
        const MAX_MATCHES: usize = 8;
        if matched.len() > MAX_MATCHES {
            matched.truncate(MAX_MATCHES);
        }

        for (name, _) in &matched {
            self.registry.promote(name);
        }

        // Audit trail: every promotion is a privilege change (a hidden tool
        // becomes callable), so record the query and what it promoted. The
        // `PermissionHook` still gates invocation, but this log is how an
        // operator reconstructs "which query surfaced tool X" after the fact.
        let promoted: Vec<String> = matched.iter().map(|(name, _)| name.clone()).collect();
        if !promoted.is_empty() {
            tracing::info!(target: "tool_search", query = %query.trim(), promoted = ?promoted, "tool_search promoted deferred tools");
        }

        // Return each match's full schema as the JSON payload.
        let tools: Vec<Value> = matched
            .iter()
            .filter_map(|(name, _)| {
                self.registry.tool_by_name(name).map(|t| {
                    let s = t.schema();
                    json!({
                        "name": s.name,
                        "description": s.description,
                        "parameters": s.parameters,
                    })
                })
            })
            .collect();

        Ok(json!({
            "matched": tools.len(),
            "promoted": promoted,
            "tools": tools,
        })
        .to_string())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tool::{Tool, ToolRegistry, ToolSchema};

    struct FakeTool {
        name: &'static str,
        desc: &'static str,
    }

    #[async_trait]
    impl Tool for FakeTool {
        fn schema(&self) -> ToolSchema {
            ToolSchema {
                name: self.name.into(),
                description: self.desc.into(),
                parameters: json!({"type": "object"}),
            }
        }
        async fn execute(&self, _args: Value) -> anyhow::Result<String> {
            Ok(format!("ran {}", self.name))
        }
    }

    fn make_registry_with_deferred() -> Arc<ToolRegistry> {
        let mut r = ToolRegistry::new();
        r.register_deferred(FakeTool {
            name: "git_status",
            desc: "Show git working tree status",
        });
        r.register_deferred(FakeTool {
            name: "git_diff",
            desc: "Show git diff between commits",
        });
        r.register_deferred(FakeTool {
            name: "http_get",
            desc: "Fetch a URL via HTTP GET",
        });
        Arc::new(r)
    }

    #[tokio::test]
    async fn tool_search_matches_by_name() {
        let registry = make_registry_with_deferred();
        let search = ToolSearch::new(registry.clone());
        let out = search.execute(json!({"query": "git"})).await.expect("execute");
        let parsed: Value = serde_json::from_str(&out).unwrap();
        assert_eq!(parsed["matched"], 2, "should match git_status + git_diff");
        // Both tools should now be promoted.
        assert!(registry.has_tool("git_status"));
        assert!(registry.has_tool("git_diff"));
        assert!(!registry.has_tool("http_get"), "http_get should remain deferred");
    }

    #[tokio::test]
    async fn tool_search_matches_by_description() {
        let registry = make_registry_with_deferred();
        let search = ToolSearch::new(registry.clone());
        let out = search.execute(json!({"query": "URL"})).await.expect("execute");
        let parsed: Value = serde_json::from_str(&out).unwrap();
        assert_eq!(parsed["matched"], 1);
        assert!(registry.has_tool("http_get"));
    }

    #[tokio::test]
    async fn tool_search_no_matches_returns_empty_list() {
        let registry = make_registry_with_deferred();
        let search = ToolSearch::new(registry);
        let out = search.execute(json!({"query": "xyzzy"})).await.expect("execute");
        let parsed: Value = serde_json::from_str(&out).unwrap();
        assert_eq!(parsed["matched"], 0);
        assert!(parsed["tools"].as_array().unwrap().is_empty());
    }

    #[tokio::test]
    async fn tool_search_rejects_missing_query() {
        let registry = make_registry_with_deferred();
        let search = ToolSearch::new(registry);
        let err = search.execute(json!({})).await.unwrap_err();
        assert!(err.to_string().contains("query"));
    }

    #[tokio::test]
    async fn promoted_tool_dispatches_through_registry() {
        let registry_arc = make_registry_with_deferred();
        let search = ToolSearch::new(registry_arc.clone());
        // Promote.
        let _ = search.execute(json!({"query": "git_status"})).await.unwrap();

        // After promotion the tool resolves via tool_by_name.
        let tool = registry_arc.tool_by_name("git_status").expect("promoted tool resolves");
        let result = tool.execute(json!({})).await.unwrap();
        assert!(result.contains("ran git_status"));
    }

    #[test]
    fn promotion_is_visible_across_clones() {
        let mut r = ToolRegistry::new();
        r.register_deferred(FakeTool { name: "alpha", desc: "first" });
        let cloned = r.clone();
        // Promote on the original — the clone shares the promoted set.
        assert!(r.promote("alpha"));
        assert!(cloned.has_tool("alpha"), "promotion must propagate across clones");
    }

    /// A promotion is observable in the returned payload (the `promoted` list),
    /// not merely as a side-effecting `tracing` log — an operator/agent can
    /// read back exactly which deferred tools a query surfaced.
    #[tokio::test]
    async fn promotion_is_observable_in_returned_list() {
        let registry = make_registry_with_deferred();
        let search = ToolSearch::new(registry);
        let out = search.execute(json!({"query": "git"})).await.expect("execute");
        let parsed: Value = serde_json::from_str(&out).unwrap();
        let promoted: Vec<String> = parsed["promoted"]
            .as_array()
            .expect("promoted list")
            .iter()
            .map(|v| v.as_str().unwrap().to_string())
            .collect();
        assert!(
            promoted.contains(&"git_status".to_string()),
            "promoted list must record git_status: {promoted:?}"
        );
        assert!(promoted.contains(&"git_diff".to_string()), "promoted list must record git_diff: {promoted:?}");
        assert!(!promoted.contains(&"http_get".to_string()), "unmatched tool must not appear: {promoted:?}");
    }

    /// **The security regression (pearl th-64b1ee).** A prompt-injection payload
    /// could make a read-only agent `tool_search` a deliberately-deferred `bash`
    /// exec tool. Promotion succeeds — but `PermissionHook`, installed on the
    /// same registry as a `ToolHook`, must STILL block the *invocation* of a
    /// dangerous command on that promoted tool. Promotion alone is inert.
    ///
    /// Asserts, using execution counters (cf. `permission::hook_gates_registry_execution`):
    /// 1. a dangerous command on the promoted `bash` is denied and its body never runs;
    /// 2. a safe command on the same promoted `bash` still runs (per-invocation gating,
    ///    not a blanket deny of everything promoted);
    /// 3. an independent safe promoted tool still runs.
    #[tokio::test]
    async fn permission_hook_gates_promoted_deferred_tool() {
        use crate::permission::{AutoMode, PermissionHook};
        use crate::tool::{ToolCall, ToolResult};
        use std::sync::atomic::{AtomicUsize, Ordering};

        struct CountingExec {
            name: &'static str,
            desc: &'static str,
            read_only: bool,
            runs: Arc<AtomicUsize>,
        }
        #[async_trait]
        impl Tool for CountingExec {
            fn schema(&self) -> ToolSchema {
                ToolSchema {
                    name: self.name.into(),
                    description: self.desc.into(),
                    parameters: json!({"type": "object", "properties": {"cmd": {"type": "string"}}}),
                }
            }
            fn is_read_only(&self) -> bool {
                self.read_only
            }
            async fn execute(&self, _args: Value) -> anyhow::Result<String> {
                self.runs.fetch_add(1, Ordering::SeqCst);
                Ok(format!("ran {}", self.name))
            }
        }

        let bash_runs = Arc::new(AtomicUsize::new(0));
        let read_runs = Arc::new(AtomicUsize::new(0));

        // An oracle/read-only-style registry: the exec tool is DEFERRED (hidden)
        // and a `PermissionHook` gates every call. `read_notes` classifies Safe.
        let mut reg = ToolRegistry::new();
        reg.register_deferred(CountingExec {
            name: "bash",
            desc: "run a shell command",
            read_only: false,
            runs: bash_runs.clone(),
        });
        reg.register_deferred(CountingExec {
            name: "read_notes",
            desc: "read project notes",
            read_only: true,
            runs: read_runs.clone(),
        });
        reg.add_hook(PermissionHook::new(AutoMode::Ask));
        let reg = Arc::new(reg);

        // Both deferred tools are invisible until tool_search promotes them.
        assert!(!reg.has_tool("bash"), "bash must start deferred (hidden)");

        // Simulate the prompt-injection: the model calls tool_search and promotes
        // the exec tool + the safe tool.
        let search = ToolSearch::new(reg.clone());
        let _ = search.execute(json!({"query": "shell"})).await.expect("promote bash");
        let _ = search.execute(json!({"query": "notes"})).await.expect("promote read_notes");
        assert!(reg.has_tool("bash"), "tool_search must have promoted bash");
        assert!(reg.has_tool("read_notes"), "tool_search must have promoted read_notes");

        let call = |name: &str, cmd: &str| ToolCall {
            id: "c1".into(),
            name: name.into(),
            arguments: json!({"cmd": cmd}),
        };

        // (1) Dangerous command on the promoted bash → PermissionHook DENIES,
        //     tool body never runs.
        let blocked: ToolResult = reg.execute(&call("bash", "rm -rf /")).await;
        assert!(blocked.is_error, "promoted dangerous bash must be blocked");
        assert!(blocked.content.contains("blocked by hook"), "content: {}", blocked.content);
        assert!(blocked.content.contains("permission denied"), "content: {}", blocked.content);
        assert_eq!(bash_runs.load(Ordering::SeqCst), 0, "denied promoted tool body MUST NOT execute");

        // (2) Safe command on the same promoted bash → still runs (per-invocation).
        let ok: ToolResult = reg.execute(&call("bash", "ls -la")).await;
        assert!(!ok.is_error, "safe command on promoted bash should run; content: {}", ok.content);
        assert_eq!(bash_runs.load(Ordering::SeqCst), 1, "safe invocation of promoted tool must execute once");

        // (3) An independent safe promoted tool still works.
        let read = reg
            .execute(&ToolCall {
                id: "c2".into(),
                name: "read_notes".into(),
                arguments: json!({}),
            })
            .await;
        assert!(!read.is_error, "safe promoted tool should run; content: {}", read.content);
        assert_eq!(read_runs.load(Ordering::SeqCst), 1, "safe promoted tool must execute");
    }
}
