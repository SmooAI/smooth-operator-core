//! Serde data-transfer objects for the activity boundary.
//!
//! A Temporal activity serializes its input and output. The engine's
//! [`LlmResponse`] is intentionally *not* `Serialize`/`Deserialize` (it carries
//! transient gateway state), so the `model_call` activity traffics
//! [`ModelCallOutput`] — a serde projection of the fields the orchestration
//! actually consumes — and the workflow-side `AgentActivities::model_call`
//! reconstructs an `LlmResponse` from it via [`ModelCallOutput::into_llm_response`].
//!
//! Everything here is plain serde over `smooth-operator-core` types and compiles
//! with **no Temporal dependency**, so it is always built and unit-tested
//! regardless of the `temporal` feature.

use serde::{Deserialize, Serialize};

use smooth_operator_core::conversation::Message;
use smooth_operator_core::llm::{LlmResponse, Usage};
use smooth_operator_core::tool::{ToolCall, ToolSchema};

/// Input to the `model_call` activity: the context window + available tools.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ModelCallInput {
    /// The conversation context window, as owned messages.
    pub messages: Vec<Message>,
    /// Tool schemas the model may call.
    pub tools: Vec<ToolSchema>,
}

/// Output of the `model_call` activity: a serde projection of [`LlmResponse`].
///
/// Carries the fields the orchestration reads (`content`, `tool_calls`,
/// `reasoning_content`) plus the accounting fields (`finish_reason`, `usage`,
/// `gateway_cost_usd`, `resolved_model`) so the durable path can preserve cost /
/// audit data. The transient `rate_limit` is dropped (it is meaningless after
/// the call returns).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ModelCallOutput {
    /// Assistant text content.
    pub content: String,
    /// Tool calls the model requested.
    pub tool_calls: Vec<ToolCall>,
    /// Provider finish reason (e.g. `stop`, `tool_calls`).
    #[serde(default)]
    pub finish_reason: String,
    /// Token usage reported by the gateway.
    #[serde(default)]
    pub usage: Usage,
    /// Authoritative gateway cost in USD, if reported.
    #[serde(default)]
    pub gateway_cost_usd: Option<f64>,
    /// Concrete upstream model the gateway resolved to, if reported.
    #[serde(default)]
    pub resolved_model: Option<String>,
    /// Reasoning/thinking content, preserved for the next request.
    #[serde(default)]
    pub reasoning_content: Option<String>,
}

impl From<&LlmResponse> for ModelCallOutput {
    fn from(r: &LlmResponse) -> Self {
        Self {
            content: r.content.clone(),
            tool_calls: r.tool_calls.clone(),
            finish_reason: r.finish_reason.clone(),
            usage: r.usage.clone(),
            gateway_cost_usd: r.gateway_cost_usd,
            resolved_model: r.resolved_model.clone(),
            reasoning_content: r.reasoning_content.clone(),
        }
    }
}

impl ModelCallOutput {
    /// Reconstruct an [`LlmResponse`] from this projection. The transient
    /// `rate_limit` is `None` (it does not survive the activity boundary).
    #[must_use]
    pub fn into_llm_response(self) -> LlmResponse {
        LlmResponse {
            content: self.content,
            tool_calls: self.tool_calls,
            finish_reason: self.finish_reason,
            usage: self.usage,
            rate_limit: None,
            gateway_cost_usd: self.gateway_cost_usd,
            resolved_model: self.resolved_model,
            reasoning_content: self.reasoning_content,
        }
    }
}

/// Input to the `tool_invoke` activity: the single tool call to execute.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ToolInvokeInput {
    /// The tool call (id + name + arguments) to dispatch.
    pub call: ToolCall,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample_response() -> LlmResponse {
        LlmResponse {
            content: "hello".into(),
            tool_calls: vec![ToolCall {
                id: "call-1".into(),
                name: "echo".into(),
                arguments: serde_json::json!({ "text": "hi" }),
            }],
            finish_reason: "tool_calls".into(),
            usage: Usage {
                prompt_tokens: 10,
                completion_tokens: 5,
                total_tokens: 15,
                cached_tokens: 2,
            },
            rate_limit: None,
            gateway_cost_usd: Some(0.0001),
            resolved_model: Some("qwen3-coder-flash".into()),
            reasoning_content: Some("because".into()),
        }
    }

    /// `LlmResponse` → DTO → `LlmResponse` preserves every field the
    /// orchestration and accounting read (rate_limit is intentionally dropped).
    #[test]
    fn model_call_output_round_trips_through_llm_response() {
        let original = sample_response();
        let dto = ModelCallOutput::from(&original);
        let restored = dto.into_llm_response();

        assert_eq!(restored.content, original.content);
        assert_eq!(restored.tool_calls.len(), 1);
        assert_eq!(restored.tool_calls[0].id, "call-1");
        assert_eq!(restored.finish_reason, "tool_calls");
        assert_eq!(restored.usage.total_tokens, 15);
        assert_eq!(restored.usage.cached_tokens, 2);
        assert_eq!(restored.gateway_cost_usd, Some(0.0001));
        assert_eq!(restored.resolved_model.as_deref(), Some("qwen3-coder-flash"));
        assert_eq!(restored.reasoning_content.as_deref(), Some("because"));
        assert!(restored.rate_limit.is_none());
    }

    /// The DTO survives a JSON serialize/deserialize round trip — i.e. it can
    /// actually cross a Temporal activity boundary.
    #[test]
    fn model_call_output_serde_round_trips() {
        let dto = ModelCallOutput::from(&sample_response());
        let json = serde_json::to_string(&dto).expect("serialize");
        let back: ModelCallOutput = serde_json::from_str(&json).expect("deserialize");
        assert_eq!(back.content, "hello");
        assert_eq!(back.tool_calls[0].name, "echo");
        assert_eq!(back.usage.prompt_tokens, 10);
    }

    /// Activity inputs serialize cleanly.
    #[test]
    fn activity_inputs_serde_round_trip() {
        let input = ModelCallInput {
            messages: vec![Message::user("hi")],
            tools: vec![ToolSchema {
                name: "echo".into(),
                description: "echo".into(),
                parameters: serde_json::json!({ "type": "object" }),
            }],
        };
        let json = serde_json::to_string(&input).expect("serialize");
        let back: ModelCallInput = serde_json::from_str(&json).expect("deserialize");
        assert_eq!(back.messages.len(), 1);
        assert_eq!(back.tools[0].name, "echo");

        let ti = ToolInvokeInput {
            call: ToolCall {
                id: "c1".into(),
                name: "echo".into(),
                arguments: serde_json::json!({}),
            },
        };
        let json = serde_json::to_string(&ti).expect("serialize");
        let back: ToolInvokeInput = serde_json::from_str(&json).expect("deserialize");
        assert_eq!(back.call.name, "echo");
    }
}
