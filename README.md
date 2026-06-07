# smooth-operator

**Polyglot AI agent orchestration core** — agents, workflows, tools, checkpointing, memory, human-in-the-loop, and cost tracking. The engine behind [smooth-agent](https://github.com/SmooAI/smooth-agent) and [lom.smoo.ai](https://lom.smoo.ai).

Inspired by LangGraph, CrewAI, and Agno — purpose-built for orchestrated agent workloads with a security-first design. The **Rust** implementation is the source of truth; TypeScript, Go, C#/.NET, and Python bindings mirror its surface.

## Repository layout

This is a multi-language SmooAI package. Each language has its own subdirectory:

| Directory | Language | Status |
| --------- | -------- | ------ |
| [`rust/`](./rust) | Rust (reference) | Active — crate `smooai-smooth-operator` |
| [`typescript/`](./typescript) | TypeScript | Planned |
| [`go/`](./go) | Go | Planned |
| [`dotnet/`](./dotnet) | C# / .NET | Planned (first-class target) |
| [`python/`](./python) | Python | Planned |

Bindings follow a **protocol-first** strategy (a stable wire spec each language implements natively), with in-process FFI (napi-rs, PyO3/uniffi) layered on where embedding the engine pays off. See [smooth-agent's architecture](https://github.com/SmooAI/smooth-agent/blob/main/docs/ARCHITECTURE.md) for the rationale.

## Rust quick start

Add to your `Cargo.toml`:

```toml
[dependencies]
smooai-smooth-operator = { git = "https://github.com/SmooAI/smooth-operator.git", branch = "main" }
```

```rust
use smooth_operator::{Agent, AgentConfig, LlmConfig, Tool, ToolRegistry, ToolSchema};
use async_trait::async_trait;

struct GetWeather;

#[async_trait]
impl Tool for GetWeather {
    fn schema(&self) -> ToolSchema {
        ToolSchema {
            name: "get_weather".into(),
            description: "Get current weather for a city".into(),
            parameters: serde_json::json!({
                "type": "object",
                "properties": { "city": { "type": "string" } },
                "required": ["city"]
            }),
        }
    }

    async fn execute(&self, args: serde_json::Value) -> anyhow::Result<String> {
        let city = args["city"].as_str().unwrap_or("unknown");
        Ok(format!("Weather in {city}: 72F, sunny"))
    }
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let llm = LlmConfig {
        api_url: "https://api.openai.com/v1".into(),
        api_key: std::env::var("OPENAI_API_KEY")?,
        model: "gpt-4o".into(),
        ..Default::default()
    };

    let config = AgentConfig::new("assistant", "You are a helpful assistant.", llm)
        .with_max_iterations(10)
        .with_parallel_tools(true);

    let mut registry = ToolRegistry::new();
    registry.register(GetWeather);

    let mut agent = Agent::new(config, registry);
    let events = agent.run("What's the weather in Tokyo?").await?;

    for event in events { println!("{event:?}"); }
    Ok(())
}
```

## Features

- **LLM Client** — OpenAI-compatible and Anthropic API support with streaming, retry policies, and rate-limit handling
- **Workflows** — `Workflow<S>` / `WorkflowBuilder<S>` graph engine (a LangGraph analog) with conditional edges
- **Tool System** — trait-based tools with pre/post hooks for surveillance, secret detection, and prompt-injection guards
- **Agent Loop** — observe-think-act cycle with configurable iteration limits, parallel tool execution, and cost budgets
- **Checkpointing** — pluggable checkpoint stores for session resume and fault tolerance (in-memory, file, SQLite via `sqlite`, Postgres via `postgres`)
- **Conversation Management** — token-aware context window with compaction strategies
- **Memory & Knowledge** — trait-based memory and knowledge-base integration (RAG seam)
- **Human-in-the-Loop** — confirmation hooks and human-input channels for gated tool calls
- **Cost Tracking** — per-model pricing, token budgets, and budget enforcement

## Cargo features

| Feature | Effect |
| ------- | ------ |
| `sqlite` | SQLite checkpoint store (`rusqlite`, bundled) |
| `postgres` | Postgres checkpoint store (r2d2 pool) |
| `bigsmooth` | Optional supervisor/reporter integration (off by default) |

## License

MIT — see [LICENSE](./LICENSE).

## Links

- [smooth-agent](https://github.com/SmooAI/smooth-agent) — the Onyx-like agent service built on this core
- [lom.smoo.ai](https://lom.smoo.ai) — run it hosted
- [smoo.ai](https://smoo.ai) — the product
- [github.com/SmooAI](https://github.com/SmooAI) — other open-source packages
