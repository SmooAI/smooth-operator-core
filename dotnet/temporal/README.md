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

What it does **not** buy you is a weaker gate. The `tool_invoke` activity dispatches through
`SmoothAgent.DispatchToolAsync`, so the worker's agent enforces the same permission mode, deny
policy, human gate and tool hooks it would enforce inline — swapping executors cannot widen what the
agent may run. Give `EngineHandles` your **configured agent**, not a bare client and tool list, or
there is no configuration for it to enforce.

## Why a separate package

The `Temporalio` dependency lives here, not in the core engine, so the published core stays
zero-infra: a consumer that never needs durable execution never pulls in the Temporal SDK. This
mirrors the Rust reference keeping its Temporal SDK behind an off-by-default `temporal` cargo
feature.

## Wiring

```csharp
// Worker side: register the workflows + activities, backed by the SAME agent you would run inline.
// Its model client backs `model_call`; its gated dispatch backs `tool_invoke`.
var engine = new EngineHandles(agent);
using var worker = new TemporalWorker(temporalClient, new TemporalWorkerOptions("smooth-operator-agent-turn")
    .AddWorkflow<AgentTurnWorkflow>()
    .AddWorkflow<HealthWorkflow>()
    .AddAllActivities(new AgentTurnActivities(engine)));
_ = worker.ExecuteAsync(workerStopToken);

// Consumer side: swap the executor — the agent is unaware. Pass a thread for multi-turn memory; the
// turn is seeded with its history and the new messages are appended back, as RunAsync would.
IAgentExecutor executor = new TemporalExecutor(temporalClient);
var thread = agent.GetNewThread();
var result = await executor.ExecuteAsync(agent, "what is the answer?", thread);
```

The **serde DTO boundary** (`Dto.cs`) is pure `System.Text.Json` over flat projections and carries no
Temporal dependency, so it is always compiled and unit-tested regardless of whether a worker runs.

MIT — © SmooAI.
