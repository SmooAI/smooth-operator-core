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

> ### The agent brain you can point at production — a single embeddable Rust crate.
>
> Most agent frameworks hand the model a pile of tools and hope. This one gives you the loop **and the brakes**: draw hard lines the model can never cross, then let it run.

`smooai-smooth-operator-core` is the agent engine itself — an observe→think→act loop over any OpenAI-compatible client, with a typed tool system, pre/post hooks, pluggable checkpoint stores, memory, RAG, human-in-the-loop gates, per-model cost budgets, and a permission gate you control. One crate; runs in a Lambda, a container, any host process. It's the runtime the [**smooth-operator**](https://github.com/SmooAI/smooth-operator) service actually ships on.

This crate is the **reference implementation** — the source of truth the [TypeScript, Python, Go, and C#/.NET ports](https://github.com/SmooAI/smooth-operator-core/blob/main/docs/Polyglot-Engines.md) mirror at parity. **The same agent brain, the same guarantees, wherever your stack already lives.** Every surface is covered by fast, offline unit tests on a deterministic `MockLlmClient`, so the loop is verified — not vibe-coded.

## Install

```bash
cargo add smooai-smooth-operator-core
cargo add tokio --features full
cargo add anyhow
```

The crate is `smooai-smooth-operator-core` (library `smooth_operator_core`), v0.14.0.

## Quickstart

A complete agent with one tool — no credentials needed — using the deterministic mock provider the engine's own tests run on. The mock is scripted to call the tool, then answer:

```rust
use std::sync::Arc;
use async_trait::async_trait;
use smooth_operator_core::{Agent, AgentConfig, LlmConfig, Tool, ToolRegistry, ToolSchema};
use smooth_operator_core::llm_provider::MockLlmClient;

struct GetWeather;

#[async_trait]
impl Tool for GetWeather {
    fn schema(&self) -> ToolSchema {
        ToolSchema {
            name: "get_weather".into(),
            description: "Get the current weather for a city".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": { "city": { "type": "string" } },
                "required": ["city"]
            }),
        }
    }

    async fn execute(&self, args: serde_json::Value) -> anyhow::Result<String> {
        Ok(format!("Weather in {}: 72F, sunny", args["city"].as_str().unwrap_or("?")))
    }
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let mock = MockLlmClient::new();
    mock.push_tool_call("call_1", "get_weather", serde_json::json!({ "city": "Tokyo" }));
    mock.push_text("It's 72F and sunny in Tokyo.");

    let mut registry = ToolRegistry::new();
    registry.register(GetWeather);

    let config = AgentConfig::new("agent", "You are a helpful assistant", LlmConfig::openrouter("fake-key"));
    let agent = Agent::new(config, registry).with_llm_provider(Arc::new(mock.clone()));

    let conversation = agent.run("what's the weather in Tokyo?").await?;
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
- **Permissions + deny-policy** — a `PermissionHook` tool-call gate (`AutoMode`: ask / accept-edits / deny-unmatched / bypass) with hard circuit-breakers (`rm -rf /`, credential paths, pipe-to-shell, dangerous domains), a persisted allow-list, and a consumer `DenyPolicy` — declarative TOML rules plus semantic predicates for what strings can't express.
- **Human-in-the-loop gate** — `ConfirmationHook` requires approval before designated tool calls run.
- **Conversation thread** — carry a conversation across multiple `run` calls.
- **`LlmProvider` seam + `MockLlmClient`** — inject any OpenAI-compatible client; the record/replay mock drives the offline tests.
- **Deferred tools + `tool_search`** — hide rarely-used tool schemas behind a built-in meta-tool the model calls to promote the ones it needs.
- **Typed workflow graph** — `Workflow<S>` / `WorkflowBuilder<S>` with conditional edges and typed state.
- **Parallel tool calls** — dispatch ≥2 tool calls concurrently (transcript order preserved).
- **Retry / backoff** — retry transient model-call failures with exponential backoff.
- **Streaming** — incremental text, tool calls, and tool results as the turn runs.

## Permissions & deny-policy — lines the agent can't cross

This is what makes an agent safe to point at real infrastructure: **you** decide what it can never do, and no prompt or model mistake talks it out of that. Every tool call passes through a gate. `AutoMode` sets the posture — read-only calls **allow**, mutating calls **ask**, dangerous calls **deny** — and hard circuit-breakers (`rm -rf /`, credential paths, pipe-to-shell, dangerous domains) fire in every mode, `Bypass` included. Attach a `DenyPolicy` on top: declarative TOML rules for the lines you can name, semantic predicates for the ones you can't. A match is a hard deny no stored grant and no mode can waive.

```rust
use std::sync::Arc;
use smooth_operator_core::{Agent, AutoMode, DenyPolicy, DenyPredicate, DenyReason, ToolCall};

// A predicate for what strings can't express — return Some(reason) to deny.
struct DenyDbWriter;
impl DenyPredicate for DenyDbWriter {
    fn evaluate(&self, call: &ToolCall) -> Option<DenyReason> {
        if call.name == "db_query" && call.arguments.to_string().contains("writer") {
            return Some(DenyReason::new("DB writer endpoint is off-limits — reads go to the replica"));
        }
        None
    }
}

// Declarative rules (TOML): never the prod AWS profile, never a prod host.
let policy = DenyPolicy::from_toml(r#"
    schema_version = 1
    [bash]
    deny_patterns = ["aws * --profile prod"]
    [network]
    deny_hosts = ["*.prod.internal"]
"#)?.with_predicate(Arc::new(DenyDbWriter));

let agent = Agent::new(config, registry)
    .with_permission_mode(AutoMode::Ask) // read allow · mutate ask · dangerous deny
    .with_deny_policy(Arc::new(policy))
    .with_extension_host(host); // the gate is installed here; set mode/policy first
```

The gate is installed by `with_extension_host` (the SEP extension host), so call `with_permission_mode` / `with_deny_policy` **before** it.

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
