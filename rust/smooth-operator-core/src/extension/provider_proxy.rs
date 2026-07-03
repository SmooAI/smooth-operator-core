//! `ExtensionLlmProvider` — an [`LlmProvider`] backed by an extension subprocess.
//!
//! Phase 7's proxied streaming: the host runs an LLM completion by sending
//! `provider/complete` to the extension that registered the provider. For a
//! streaming call the extension emits `provider/delta` notifications (each a
//! serialized [`StreamEvent`]) keyed by a `request_id` while it works, then
//! replies to the request with the final [`ProviderCompleteResult`]. This
//! adapter routes those deltas onto an `LlmEventStream` and terminates it when
//! the request resolves — so an extension-registered provider is a drop-in for
//! the native [`LlmClient`](crate::llm::LlmClient) at the agent-loop seam.
//!
//! Delta correlation lives in [`ProviderStreams`], a shared map the host's
//! [`HostInbound`](super::host) notification handler writes into and this
//! adapter reads from. `request_id`s are UUIDs, unique across every provider and
//! process, so one shared map is enough.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use futures_util::stream::StreamExt;
use serde_json::Value;
use tokio::sync::mpsc;
use tokio_stream::wrappers::UnboundedReceiverStream;

use super::process::ExtensionProcess;
use super::protocol::{method, Context, ProviderCompleteParams, ProviderCompleteResult};
use crate::conversation::Message;
use crate::llm::{LlmResponse, ResponseFormat, StreamEvent};
use crate::llm_provider::{LlmEventStream, LlmProvider};
use crate::tool::ToolSchema;

/// Upper bound for a single `provider/complete` round-trip. Generous: a slow
/// upstream model can take a while; the agent's own turn budget bounds it above.
const PROVIDER_COMPLETE_TIMEOUT: Duration = Duration::from_secs(300);

/// One item the delta lane carries to the streaming adapter.
enum StreamMsg {
    /// A `provider/delta` chunk (a serialized [`StreamEvent`]).
    Delta(Value),
    /// The `provider/complete` request resolved — carries the final result so the
    /// adapter can emit trailing tool-calls/usage and a terminal `Done`.
    Final(anyhow::Result<ProviderCompleteResult>),
}

/// Shared `request_id` → delta sink registry, written by the host's inbound
/// notification handler and read by [`ExtensionLlmProvider::chat_stream`].
#[derive(Clone, Default)]
pub struct ProviderStreams {
    inner: Arc<Mutex<HashMap<String, mpsc::UnboundedSender<StreamMsg>>>>,
}

impl std::fmt::Debug for ProviderStreams {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        let n = self.inner.lock().map(|m| m.len()).unwrap_or(0);
        f.debug_struct("ProviderStreams").field("open", &n).finish()
    }
}

impl ProviderStreams {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, HashMap<String, mpsc::UnboundedSender<StreamMsg>>> {
        self.inner.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    /// Register a delta sink for `request_id`, returning the receiving half.
    fn open(&self, request_id: &str) -> (mpsc::UnboundedSender<StreamMsg>, mpsc::UnboundedReceiver<StreamMsg>) {
        let (tx, rx) = mpsc::unbounded_channel();
        self.lock().insert(request_id.to_string(), tx.clone());
        (tx, rx)
    }

    fn remove(&self, request_id: &str) {
        self.lock().remove(request_id);
    }

    /// Route a `provider/delta` notification's `event` to its stream. No-op if
    /// the request already finished (a late delta after the terminal reply). The
    /// host calls this from its ext→host notification handler.
    pub fn route_delta(&self, request_id: &str, event: Value) {
        if let Some(tx) = self.lock().get(request_id) {
            let _ = tx.send(StreamMsg::Delta(event));
        }
    }
}

/// An [`LlmProvider`] that proxies through an extension's registered provider.
pub struct ExtensionLlmProvider {
    process: Arc<ExtensionProcess>,
    streams: ProviderStreams,
    provider: String,
    model: String,
    context: Context,
    /// Reasoning/thinking level applied to every request (from `session/set_model`).
    thinking: Option<String>,
}

impl std::fmt::Debug for ExtensionLlmProvider {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ExtensionLlmProvider")
            .field("provider", &self.provider)
            .field("model", &self.model)
            .field("thinking", &self.thinking)
            .finish()
    }
}

impl ExtensionLlmProvider {
    #[must_use]
    pub fn new(process: Arc<ExtensionProcess>, streams: ProviderStreams, provider: impl Into<String>, model: impl Into<String>, context: Context) -> Self {
        Self {
            process,
            streams,
            provider: provider.into(),
            model: model.into(),
            context,
            thinking: None,
        }
    }

    /// Set the reasoning/thinking level applied to subsequent completions.
    #[must_use]
    pub fn with_thinking(mut self, thinking: Option<String>) -> Self {
        self.thinking = thinking;
        self
    }

    #[must_use]
    pub fn provider(&self) -> &str {
        &self.provider
    }

    #[must_use]
    pub fn model(&self) -> &str {
        &self.model
    }

    fn params(&self, request_id: &str, messages: &[&Message], tools: &[ToolSchema], stream: bool, response_format: Option<&ResponseFormat>) -> Value {
        let p = ProviderCompleteParams {
            request_id: request_id.to_string(),
            provider: self.provider.clone(),
            model: self.model.clone(),
            messages: messages.iter().map(|m| serde_json::to_value(m).unwrap_or(Value::Null)).collect(),
            tools: tools.iter().map(|t| serde_json::to_value(t).unwrap_or(Value::Null)).collect(),
            stream,
            response_format: response_format.map(response_format_to_wire),
            thinking: self.thinking.clone(),
        };
        // Injects `context` so the extension can see the dispatch tier/epoch. The
        // typed params carry no `context` (host→ext requests attach it uniformly),
        // so merge it in here.
        let mut v = serde_json::to_value(p).unwrap_or(Value::Null);
        if let Value::Object(map) = &mut v {
            map.insert("context".to_string(), serde_json::to_value(&self.context).unwrap_or(Value::Null));
        }
        v
    }

    /// Non-streaming `provider/complete`.
    async fn complete(&self, request_id: &str, params: Value) -> anyhow::Result<ProviderCompleteResult> {
        let raw = self.process.request(method::PROVIDER_COMPLETE, params, PROVIDER_COMPLETE_TIMEOUT).await?;
        // A late delta for a non-streamed request never opened a stream, so no
        // cleanup is needed here.
        let _ = request_id;
        serde_json::from_value(raw).map_err(|e| anyhow::anyhow!("malformed provider/complete result: {e}"))
    }
}

/// Render a [`ResponseFormat`] into the OpenAI-compatible `response_format` wire
/// object the engine sends real providers, so an extension provider sees the
/// same shape: `{"type":"json_schema","json_schema":{name,schema,strict}}`.
fn response_format_to_wire(format: &ResponseFormat) -> Value {
    match format {
        ResponseFormat::JsonSchema { name, schema, strict } => serde_json::json!({
            "type": "json_schema",
            "json_schema": { "name": name, "schema": schema, "strict": strict },
        }),
    }
}

/// Map a [`ProviderCompleteResult`] onto the engine's [`LlmResponse`].
fn to_llm_response(r: ProviderCompleteResult) -> LlmResponse {
    LlmResponse {
        content: r.content,
        tool_calls: r.tool_calls,
        finish_reason: r.finish_reason,
        usage: r.usage,
        rate_limit: None,
        gateway_cost_usd: None,
        resolved_model: r.resolved_model,
        reasoning_content: r.reasoning_content,
    }
}

/// The trailing events a final result contributes once the delta chunks are
/// drained: any tool calls the extension reported (as `ToolCallStart` +
/// `ToolCallArgumentsDelta` so a streaming consumer accumulates them the same way
/// it would a native stream), a usage event, and the terminal `Done`.
fn final_events(r: &ProviderCompleteResult) -> Vec<StreamEvent> {
    let mut out = Vec::new();
    for (i, call) in r.tool_calls.iter().enumerate() {
        out.push(StreamEvent::ToolCallStart {
            index: i,
            id: call.id.clone(),
            name: call.name.clone(),
        });
        // Arguments as a single chunk — the extension already has the full call.
        if let Ok(args) = serde_json::to_string(&call.arguments) {
            out.push(StreamEvent::ToolCallArgumentsDelta {
                index: i,
                arguments_chunk: args,
            });
        }
    }
    out.push(StreamEvent::Usage(r.usage.clone()));
    if let Some(model) = &r.resolved_model {
        out.push(StreamEvent::Model { name: model.clone() });
    }
    out.push(StreamEvent::Done {
        finish_reason: r.finish_reason.clone(),
    });
    out
}

#[async_trait]
impl LlmProvider for ExtensionLlmProvider {
    async fn chat(&self, messages: &[&Message], tools: &[ToolSchema]) -> anyhow::Result<LlmResponse> {
        let request_id = uuid::Uuid::new_v4().to_string();
        let params = self.params(&request_id, messages, tools, false, None);
        let result = self.complete(&request_id, params).await?;
        Ok(to_llm_response(result))
    }

    async fn chat_structured(&self, messages: &[&Message], format: &ResponseFormat) -> anyhow::Result<LlmResponse> {
        let request_id = uuid::Uuid::new_v4().to_string();
        let params = self.params(&request_id, messages, &[], false, Some(format));
        let result = self.complete(&request_id, params).await?;
        Ok(to_llm_response(result))
    }

    async fn chat_stream(&self, messages: &[&Message], tools: &[ToolSchema]) -> anyhow::Result<LlmEventStream> {
        let request_id = uuid::Uuid::new_v4().to_string();
        let (terminal_tx, rx) = self.streams.open(&request_id);
        let params = self.params(&request_id, messages, tools, true, None);

        // Drive the request concurrently with delta delivery: the `request()`
        // future only resolves after the extension sends its final reply frame,
        // which the reader task processes AFTER every earlier `provider/delta`
        // line — so all Delta messages are enqueued (in order) before Final.
        let process = Arc::clone(&self.process);
        let streams = self.streams.clone();
        let rid = request_id.clone();
        tokio::spawn(async move {
            let result = process
                .request(method::PROVIDER_COMPLETE, params, PROVIDER_COMPLETE_TIMEOUT)
                .await
                .and_then(|raw| serde_json::from_value::<ProviderCompleteResult>(raw).map_err(|e| anyhow::anyhow!("malformed provider/complete result: {e}")));
            // Stop routing deltas before sending Final so a late delta can't wedge
            // in after the terminal marker.
            streams.remove(&rid);
            let _ = terminal_tx.send(StreamMsg::Final(result));
        });

        // Adapt the StreamMsg lane into an LlmEventStream. `flat_map` because the
        // Final message expands into several trailing StreamEvents.
        let stream = UnboundedReceiverStream::new(rx).flat_map(|msg| {
            let events: Vec<anyhow::Result<StreamEvent>> = match msg {
                StreamMsg::Delta(event) => match serde_json::from_value::<StreamEvent>(event) {
                    Ok(ev) => vec![Ok(ev)],
                    // A malformed delta is dropped with a trace rather than tearing
                    // the whole stream down — the Final marker still terminates it.
                    Err(e) => {
                        tracing::warn!(error = %e, "provider/delta: undecodable stream event, dropping");
                        vec![]
                    }
                },
                StreamMsg::Final(Ok(result)) => final_events(&result).into_iter().map(Ok).collect(),
                StreamMsg::Final(Err(e)) => vec![Err(e)],
            };
            tokio_stream::iter(events)
        });

        Ok(stream.boxed())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::tool::ToolCall;

    #[test]
    fn to_llm_response_maps_every_field() {
        let r = ProviderCompleteResult {
            content: "hi".into(),
            tool_calls: vec![ToolCall {
                id: "c1".into(),
                name: "search".into(),
                arguments: serde_json::json!({"q": "x"}),
            }],
            finish_reason: "tool_calls".into(),
            usage: crate::llm::Usage {
                prompt_tokens: 10,
                completion_tokens: 5,
                total_tokens: 15,
                cached_tokens: 0,
            },
            reasoning_content: Some("thinking".into()),
            resolved_model: Some("gpt-x".into()),
        };
        let resp = to_llm_response(r);
        assert_eq!(resp.content, "hi");
        assert_eq!(resp.tool_calls.len(), 1);
        assert_eq!(resp.finish_reason, "tool_calls");
        assert_eq!(resp.usage.total_tokens, 15);
        assert_eq!(resp.reasoning_content.as_deref(), Some("thinking"));
        assert_eq!(resp.resolved_model.as_deref(), Some("gpt-x"));
    }

    #[test]
    fn final_events_emits_tool_calls_usage_and_done() {
        let r = ProviderCompleteResult {
            content: String::new(),
            tool_calls: vec![ToolCall {
                id: "c1".into(),
                name: "get_weather".into(),
                arguments: serde_json::json!({"city": "SF"}),
            }],
            finish_reason: "tool_calls".into(),
            usage: crate::llm::Usage::default(),
            reasoning_content: None,
            resolved_model: Some("m".into()),
        };
        let events = final_events(&r);
        // ToolCallStart, ToolCallArgumentsDelta, Usage, Model, Done
        assert!(matches!(events[0], StreamEvent::ToolCallStart { index: 0, .. }));
        assert!(matches!(events[1], StreamEvent::ToolCallArgumentsDelta { index: 0, .. }));
        assert!(matches!(events[2], StreamEvent::Usage(_)));
        assert!(matches!(events[3], StreamEvent::Model { .. }));
        assert!(matches!(events.last(), Some(StreamEvent::Done { .. })));
    }

    #[test]
    fn final_events_plain_text_is_just_usage_and_done() {
        let r = ProviderCompleteResult {
            content: "hello".into(),
            finish_reason: "stop".into(),
            ..Default::default()
        };
        let events = final_events(&r);
        // No tool calls, no resolved_model → Usage then Done.
        assert_eq!(events.len(), 2);
        assert!(matches!(events[0], StreamEvent::Usage(_)));
        assert!(matches!(events[1], StreamEvent::Done { .. }));
    }

    #[test]
    fn route_delta_is_noop_for_unknown_request() {
        let streams = ProviderStreams::new();
        // No panic, no effect — a late/unknown delta is dropped.
        streams.route_delta("nope", serde_json::json!({"type": "Delta", "content": "x"}));
        assert_eq!(streams.lock().len(), 0);
    }

    #[tokio::test]
    async fn route_delta_reaches_the_open_stream() {
        let streams = ProviderStreams::new();
        let (_tx, mut rx) = streams.open("req-1");
        streams.route_delta("req-1", serde_json::json!({"type": "Delta", "content": "hi"}));
        match rx.recv().await {
            Some(StreamMsg::Delta(v)) => assert_eq!(v["content"], "hi"),
            other => panic!("expected a delta, got {:?}", other.is_some()),
        }
        // After remove, further deltas are dropped.
        streams.remove("req-1");
        streams.route_delta("req-1", serde_json::json!({"type": "Delta", "content": "late"}));
        // Only the first delta was delivered.
    }
}
