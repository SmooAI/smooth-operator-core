# smooai-smooth-operator-temporal

Optional [Temporal](https://temporal.io) durable-execution backend for the
[`smooai-smooth-operator-core`](https://crates.io/crates/smooai-smooth-operator-core)
agent engine (ADR-030).

An agent turn runs as a Temporal **workflow** whose side-effects — the model call
and each tool invocation — are Temporal **activities**. The workflow drives the
engine's deterministic `drive_turn` orchestration unchanged, so the durable path
and the in-process path are the *same loop*. That buys crash-safe resume, durable
human-in-the-loop, and durable timers without a second implementation of the
agent loop to keep in sync.

## Feature gating

The Temporal SDK and all workflow/activity wiring live behind the **`temporal`**
feature, **off by default**. Without it this crate compiles only its serde DTO
boundary and pulls no `temporalio-*` dependency, so a consumer that does not opt
in stays zero-infra.

```toml
smooai-smooth-operator-temporal = { version = "1.8", features = ["temporal"] }
```

## What it gives you

- **Crash-safe resume** — the workflow's event history is the checkpoint, so a
  worker restart mid-turn resumes rather than restarting the turn.
- **Durable human-in-the-loop** — a tool listed in `approval_required_tools`
  blocks on `wait_condition` until an `approve_tool` / `deny_tool` signal names
  its call id. The pending decision lives in workflow history, so it survives a
  disconnected client and can resolve hours later. A denial returns a tool-error
  result to the model without ever executing the tool.
- **Durable timers** — a call to the configured `wait_tool` sleeps the workflow
  on a Temporal timer, a pause that can span days.

## Usage

Register the workflow and activities on a worker, and install the engine handles
the activities run against once at startup:

```rust
use std::sync::Arc;
use smooai_smooth_operator_temporal::temporal::{init_engine, EngineHandles};
use smooth_operator_core::tool::ToolRegistry;

init_engine(EngineHandles {
    llm: Arc::new(my_llm_provider),
    tools: Arc::new(ToolRegistry::new()),
})
.map_err(|_| "engine handles already initialized")?;
```

Activities are registered as free functions, so the model provider and tool
registry they need are held process-globally rather than per instance. A worker
that never calls `init_engine` fails its activities loudly as non-retryable
instead of silently no-oping.

See `AgentTurnWorkflow` / `AgentTurnActivities` for the workflow surface and
`AgentTurnInput` for what a turn is seeded with.

## Status

The pinned SDK (`=0.4.0`) is a **preview** release, matching the SmooAI
platform's Temporal worker (ADR-029). Expect the pin to move.

## License

MIT
