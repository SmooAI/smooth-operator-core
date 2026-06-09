//! Compile-guard for the code snippets in the repo-root `README.md`.
//!
//! The README's Quickstart and Showcase are the front door of the crate; if
//! they drift from the real API (renamed builders, removed re-exports, changed
//! signatures) a fresh reader hits a wall on line one. These two modules mirror
//! those snippets verbatim, so `cargo test` fails the moment the README would
//! stop compiling. Keep them in sync when you edit README.md.

#![allow(dead_code, unused_variables, unused_imports)]

// ===== Quickstart (README.md "Quickstart") =====
mod quickstart {
    use async_trait::async_trait;
    use smooth_operator_core::{Agent, AgentConfig, LlmConfig, Role, Tool, ToolRegistry, ToolSchema};

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

    async fn run_quickstart() -> anyhow::Result<()> {
        let llm = LlmConfig::openrouter(std::env::var("OPENROUTER_API_KEY")?).with_model("openai/gpt-4o");

        let config = AgentConfig::new("assistant", "You are a helpful assistant.", llm)
            .with_max_iterations(10)
            .with_parallel_tools(true);

        let mut registry = ToolRegistry::new();
        registry.register(GetWeather);

        let agent = Agent::new(config, registry);
        let conversation = agent.run("What's the weather in Tokyo?").await?;

        if let Some(answer) = conversation.messages.iter().rev().find(|m| m.role == Role::Assistant) {
            println!("{}", answer.content);
        }
        Ok(())
    }
}

// ===== Showcase (README.md "Showcase: a checkpointed workflow with HITL and a cost budget") =====
mod showcase {
    use std::sync::Arc;
    use std::time::Duration;

    use smooth_operator_core::{
        human_channel, Agent, AgentConfig, ConfirmationHook, CostBudget, HumanResponse, LlmConfig, MemoryCheckpointStore, ToolRegistry,
    };

    fn build() -> anyhow::Result<()> {
        let llm = LlmConfig::openrouter(std::env::var("OPENROUTER_API_KEY").unwrap_or_default());
        let mut registry = ToolRegistry::new();

        // 1. Persist progress so a crashed turn resumes instead of restarting.
        let checkpoints = Arc::new(MemoryCheckpointStore::default());

        // 2. Cap spend per session.
        let budget = CostBudget {
            max_cost_usd: Some(0.50),
            max_tokens: None,
        };

        // 3. Gate write/irreversible tools behind a human "yes".
        let channels = human_channel();
        let confirm = ConfirmationHook::new(
            vec!["delete_".into(), "send_".into()],
            channels.request_tx,
            channels.response_rx,
            Duration::from_secs(300),
        );
        registry.add_hook(confirm);

        let mut requests = channels.request_rx;
        let responses = channels.response_tx;
        tokio::spawn(async move {
            while let Some(req) = requests.recv().await {
                let _ = responses.send(HumanResponse::Approved);
            }
        });

        let config = AgentConfig::new("assistant", "You are a careful assistant.", llm).with_budget(budget);
        let agent = Agent::new(config, registry).with_checkpoint_store(checkpoints);
        let _ = agent;
        Ok(())
    }
}

#[test]
fn readme_snippets_compile() {
    // The value of this test is that the module bodies above type-check.
    // Reaching here means the README's public-API surface still resolves.
}
