//! `ExtensionTool` — a [`Tool`] backed by an extension subprocess.
//!
//! Registered tools appear to the agent as ordinary tools named
//! `<extension>.<tool>` (the MCP convention). `execute` forwards to the
//! extension over `tool/execute` and maps the reply back to a `ToolResult`.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use serde_json::json;

use super::process::ExtensionProcess;
use super::protocol::{method, Context, ToolExecuteResult, ToolRegistration};
use crate::tool::{Tool, ToolSchema};

/// Upper bound for a single `tool/execute` round-trip. The agent's
/// `ToolRegistry` also applies its own per-tool timeout; whichever is shorter
/// wins in practice.
const TOOL_EXECUTE_TIMEOUT: Duration = Duration::from_secs(120);

/// A tool exposed by an extension.
#[derive(Debug)]
pub struct ExtensionTool {
    /// `<extension>.<tool>` — what the agent/LLM sees.
    dotted_name: String,
    /// Bare tool name sent to the extension.
    bare_name: String,
    description: String,
    parameters: serde_json::Value,
    process: Arc<ExtensionProcess>,
    context: Context,
}

impl ExtensionTool {
    #[must_use]
    pub fn new(ext_name: &str, reg: &ToolRegistration, process: Arc<ExtensionProcess>, context: Context) -> Self {
        Self {
            dotted_name: format!("{ext_name}.{}", reg.name),
            bare_name: reg.name.clone(),
            description: reg.description.clone(),
            parameters: reg.parameters.clone(),
            process,
            context,
        }
    }

    #[must_use]
    pub fn name(&self) -> &str {
        &self.dotted_name
    }
}

#[async_trait]
impl Tool for ExtensionTool {
    fn schema(&self) -> ToolSchema {
        ToolSchema {
            name: self.dotted_name.clone(),
            description: self.description.clone(),
            parameters: self.parameters.clone(),
        }
    }

    async fn execute(&self, arguments: serde_json::Value) -> anyhow::Result<String> {
        let call_id = uuid::Uuid::new_v4().to_string();
        let params = json!({
            "call_id": call_id,
            "tool": self.bare_name,
            "arguments": arguments,
            "context": self.context,
        });
        let raw = self.process.request(method::TOOL_EXECUTE, params, TOOL_EXECUTE_TIMEOUT).await?;
        let result: ToolExecuteResult = serde_json::from_value(raw).map_err(|e| anyhow::anyhow!("malformed tool/execute result: {e}"))?;
        if result.is_error {
            anyhow::bail!("{}", result.content);
        }
        // ponytail: `details` is dropped here — Tool::execute returns only a
        // String. Surfacing structured details rides on ToolCallUpdate/event
        // wiring in a later phase; stash lives in ToolExecuteResult already.
        Ok(result.content)
    }

    fn is_concurrent_safe(&self) -> bool {
        // Extensions run in their own process with a per-extension ordered
        // stream; treat their tools as non-parallel-safe so the registry
        // serializes them (conservative until an extension opts in).
        false
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::extension::process::{DefaultInboundHandler, SpawnSpec};
    use crate::extension::protocol::Tier;
    use std::collections::HashMap;

    // A throwaway process just so we can construct an ExtensionTool; `schema()`
    // never touches it. `cat` needs no `CARGO_BIN_EXE`, so this stays a unit
    // test. The live execute path is in `tests/sep_process.rs`.
    fn dummy_process() -> Arc<ExtensionProcess> {
        let spec = SpawnSpec {
            command: "cat".into(),
            args: vec![],
            env: HashMap::new(),
            cwd: None,
        };
        Arc::new(ExtensionProcess::spawn(spec, Arc::new(DefaultInboundHandler)).expect("spawn cat"))
    }

    fn say_registration() -> ToolRegistration {
        ToolRegistration {
            name: "say".into(),
            description: "Echo a phrase back.".into(),
            parameters: json!({"type": "object", "properties": {"phrase": {"type": "string"}}, "required": ["phrase"]}),
            deferred: false,
        }
    }

    // tokio runtime required: `dummy_process` spawns a child (`cat`).
    #[tokio::test]
    async fn schema_uses_dotted_name() {
        let tool = ExtensionTool::new(
            "echo",
            &say_registration(),
            dummy_process(),
            Context {
                token: "t".into(),
                tier: Tier::Command,
            },
        );
        assert_eq!(tool.schema().name, "echo.say");
        assert_eq!(tool.name(), "echo.say");
        assert!(!tool.is_concurrent_safe());
    }
}
