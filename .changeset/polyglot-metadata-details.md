---
'@smooai/smooth-operator-core': patch
---

Polyglot parity: LLM request metadata + structured tool details for the Go, TypeScript, Python, and .NET engines.

- **Request metadata** (Rust `ChatRequest.metadata` / `with_metadata` parity): each engine's agent options gain a `metadata` map forwarded verbatim as the OpenAI-compatible request's top-level `metadata` field on every model call — LiteLLM records it on spend logs, so hosts get per-agent LLM cost attribution at the gateway. Unset and empty are byte-identical on the wire (nothing is sent).
- **Structured tool details** (Rust `AgentEvent::ToolCallComplete.details` parity): a post-call tool hook may attach a structured, UI-facing payload to the mutable `ToolResult`; the streaming tool-result event now forwards it verbatim and un-truncated (Go `StreamEvent.Details`, TS `tool_result.details`, Python `ToolResultEvent.details`). The model only ever sees the text content. In .NET the seam already exists via `AIContent.AdditionalProperties` on the mutable `FunctionResultContent` — now covered by a test documenting it.
