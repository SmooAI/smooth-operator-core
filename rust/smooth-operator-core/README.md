<p align="center">
  <a href="https://smoo.ai"><img src="https://raw.githubusercontent.com/SmooAI/smooth-operator-core/main/.github/banner.png" alt="smooth-operator-core — The Rust engine for orchestrated AI agents" width="100%" /></a>
</p>

<p align="center">
  <a href="https://smoo.ai/th"><img src="https://img.shields.io/badge/Smoo_AI-platform-00A6A6?style=for-the-badge&labelColor=020618" alt="Smoo AI"></a>
  <a href="https://github.com/SmooAI/smooth-operator-core/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-MIT-F49F0A?style=for-the-badge&labelColor=020618" alt="license"></a>
  <a href="https://lom.smoo.ai"><img src="https://img.shields.io/badge/hosted-lom.smoo.ai-FF6B6C?style=for-the-badge&labelColor=020618" alt="lom.smoo.ai"></a>
</p>

<p align="center">
  <a href="https://crates.io/crates/smooai-smooth-operator-core"><img src="https://img.shields.io/crates/v/smooai-smooth-operator-core?style=flat-square&color=00A6A6&labelColor=020618" alt="crates.io"></a>
  <img src="https://img.shields.io/badge/Rust-reference%20impl-FF6B6C?style=flat-square&labelColor=020618" alt="Rust reference implementation">
</p>

---

> The agent runtime behind the [smooth-operator](https://github.com/SmooAI/smooth-operator) service and [lom.smoo.ai](https://lom.smoo.ai). Agents, workflows, tools, checkpointing, memory, human-in-the-loop, and per-model cost budgets — as a single embeddable Rust crate. It's the engine, not a notebook demo.

`smooai-smooth-operator-core` is the **reference implementation** of the Smoo AI agent engine — the observe→think→act loop that powers the [**smooth-operator**](https://github.com/SmooAI/smooth-operator) service and [**lom.smoo.ai**](https://lom.smoo.ai). It gives you the moving parts of a serious agent framework — a typed tool system, a graph workflow engine, pluggable checkpoint stores, memory, RAG, human-in-the-loop gates, and per-model cost budgets — as one embeddable crate.

This Rust crate is the source of truth: the [TypeScript, Python, Go, and C#/.NET engines](https://github.com/SmooAI/smooth-operator-core/blob/main/docs/Polyglot-Engines.md) mirror its behavior. Every surface is covered by **fast, offline unit tests** built on a deterministic `MockLlmClient`, so the loop is verified — not vibe-coded.

## Install

```bash
cargo add smooai-smooth-operator-core
cargo add tokio --features full
cargo add anyhow
```

The crate is `smooai-smooth-operator-core` (library `smooth_operator_core`), v0.14.0.

## Quickstart

A complete agent — no credentials needed — using the deterministic mock provider the engine's own tests run on:

```rust
use std::sync::Arc;
use smooth_operator_core::{Agent, AgentConfig, LlmConfig, ToolRegistry};
use smooth_operator_core::llm_provider::MockLlmClient;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let mock = MockLlmClient::new();
    mock.push_text("the answer is 42");

    let config = AgentConfig::new("agent", "You are a helpful assistant", LlmConfig::openrouter("fake-key"));
    let agent = Agent::new(config, ToolRegistry::new())
        .with_llm_provider(Arc::new(mock.clone()));

    let conversation = agent.run("what is the answer?").await?;
    println!("{}", conversation.last_assistant_content().unwrap_or(""));
    Ok(())
}
```

`with_llm_provider` injects any `Arc<dyn LlmProvider>` (here the mock); without it, `run` builds a real OpenAI-compatible `LlmClient` from `config.llm` — point `LlmConfig.api_url` at OpenAI, an Anthropic-compatible proxy, vLLM, Ollama's shim, or your own gateway (`https://llm.smoo.ai/v1`). `run` returns the full `Conversation`; `last_assistant_content()` is the final answer.

## Features

The full parity surface — every engine in the [polyglot set](https://github.com/SmooAI/smooth-operator-core/blob/main/docs/Polyglot-Engines.md) ships it, and this crate defines it:

- **Agentic tool-calling loop** — observe→think→act with iteration caps and a typed `AgentEvent` stream.
- **Typed tools + guardrails** — `Tool` trait + `ToolRegistry`, with pre/post hooks for surveillance, secret detection, and prompt-injection guards.
- **Knowledge / RAG + vectors** — `KnowledgeBase` trait grounds each turn in retrieved documents.
- **Memory** — long-term entries recalled into context each turn.
- **Compaction** — a sliding-window token budget keeps the prompt under a ceiling.
- **Cost / budget** — per-model `ModelPricing`, `CostBudget`, `CostTracker` with hard enforcement.
- **Checkpointing** — `CheckpointStore`: in-memory, SQLite (`sqlite` feature), or Postgres (`postgres` feature) for resume.
- **Rerank** — rerank retrieved hits before injection (lexical reranker built in).
- **Sub-agents / delegation** — spawn child agents for sub-tasks.
- **Cast + clearance** — roles with per-role tool-access policy.
- **Human-in-the-loop gate** — `ConfirmationHook` requires approval before designated tool calls run.
- **Conversation thread** — carry a conversation across multiple `run` calls.
- **`LlmProvider` seam + `MockLlmClient`** — inject any OpenAI-compatible client; the record/replay mock drives the offline tests.
- **Deferred tools + `tool_search`** — hide rarely-used tool schemas behind a built-in meta-tool the model calls to promote the ones it needs.
- **Typed workflow graph** — `Workflow<S>` / `WorkflowBuilder<S>` with conditional edges and typed state.
- **Parallel tool calls** — dispatch ≥2 tool calls concurrently (transcript order preserved).
- **Retry / backoff** — retry transient model-call failures with exponential backoff.
- **Streaming** — incremental text, tool calls, and tool results as the turn runs.

## Streaming

For live token deltas, tool-call, and tool-result events, drive the agent with `run_with_channel(msg, tx)` and consume the `AgentEvent` stream off the receiver — instead of `run`, which returns a single completed `Conversation`.

## Part of Smoo AI

`smooth-operator-core` is built and open-sourced by **[Smoo AI](https://smoo.ai)** — the AI-powered business platform with AI built into every product: CRM, customer support, campaigns, field service, observability, and developer tools.

- 🚀 **Smooth on the platform** — [smoo.ai/th](https://smoo.ai/th)
- 🧰 **More open source from Smoo AI** — [smoo.ai/open-source](https://smoo.ai/open-source)
- 🧩 **Run it hosted** — [lom.smoo.ai](https://lom.smoo.ai)

## Links

- [**lom.smoo.ai**](https://lom.smoo.ai) — run it hosted
- [smooth-operator-core](https://github.com/SmooAI/smooth-operator-core) — the polyglot engine repo
- [Polyglot Engines](https://github.com/SmooAI/smooth-operator-core/blob/main/docs/Polyglot-Engines.md) — install + hello-agent in all five languages
- [smoo.ai](https://smoo.ai) — the product · [smoo.ai/open-source](https://smoo.ai/open-source) — more open source

## License

MIT — see [LICENSE](https://github.com/SmooAI/smooth-operator-core/blob/main/LICENSE).

---

<p align="center">
  Built by <a href="https://smoo.ai"><strong>Smoo AI</strong></a> — AI built into every product.
</p>
