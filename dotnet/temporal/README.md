# SmooAI.SmoothOperator.Temporal

Optional **Temporal-backed durable execution** backend for
[`SmooAI.SmoothOperator.Core`](https://www.nuget.org/packages/SmooAI.SmoothOperator.Core)
(ADR-030). The C# sibling of the Rust reference's separate
[`smooth-operator-temporal`](https://github.com/SmooAI/smooth-operator-core/tree/main/rust/smooth-operator-temporal)
crate.

An agent turn runs as a Temporal **workflow** (`AgentTurnWorkflow`) whose side-effects — the model
call and each tool invocation — are Temporal **activities** (`AgentTurnActivities`). The workflow
drives the engine's deterministic `AgentTurn.DriveTurnAsync` orchestration **unchanged**, so the
durable path and the in-process path are the _same loop_ — one loop, two backends.

What durable execution buys you over the in-process `InProcessExecutor`:

- **Crash-safe resume** — a turn survives a worker restart mid-flight.
- **Durable human-in-the-loop** — an approval-gated tool blocks on an `ApproveTool` / `DenyTool`
  signal recorded in workflow history, so the pause outlives a browser refresh and can resolve hours
  later.
- **Durable timers** — a `wait` tool sleeps the workflow on a Temporal timer that can span days,
  letting an agent schedule its own follow-up.

## Why a separate package

The `Temporalio` dependency lives here, not in the core engine, so the published core stays
zero-infra: a consumer that never needs durable execution never pulls in the Temporal SDK. This
mirrors the Rust reference keeping its Temporal SDK behind an off-by-default `temporal` cargo
feature.

## Wiring

```csharp
// Worker side: register the workflows + activities, backed by your model client and tools.
var engine = new EngineHandles(chatClient, tools);
using var worker = new TemporalWorker(temporalClient, new TemporalWorkerOptions("smooth-operator-agent-turn")
    .AddWorkflow<AgentTurnWorkflow>()
    .AddWorkflow<HealthWorkflow>()
    .AddAllActivities(new AgentTurnActivities(engine)));
_ = worker.ExecuteAsync(workerStopToken);

// Consumer side: swap the executor — the agent is unaware.
IAgentExecutor executor = new TemporalExecutor(temporalClient);
var result = await executor.ExecuteAsync(agent, "what is the answer?");
```

The **serde DTO boundary** (`Dto.cs`) is pure `System.Text.Json` over flat projections and carries no
Temporal dependency, so it is always compiled and unit-tested regardless of whether a worker runs.

MIT — © SmooAI.
