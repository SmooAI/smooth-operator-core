# smooth-operator-core/go/temporal

Optional [Temporal](https://temporal.io) durable-execution backend for the
`smooth-operator` **Go** agent engine
([`github.com/SmooAI/smooth-operator-core/go`](https://github.com/SmooAI/smooth-operator-core))
— ADR-030. The Go sibling of the Rust `smooth-operator-temporal` crate.

An agent turn runs as a Temporal **workflow** whose side-effects — the model call
and each tool invocation — are Temporal **activities**. The workflow drives the
engine's deterministic `core.DriveTurn` orchestration **unchanged**, so the
durable path and the in-process path are the *same loop*. That buys crash-safe
resume, durable human-in-the-loop, and durable timers without a second
implementation of the agent loop to keep in sync.

## Optional by construction

This is a **separate Go module**, the mirror of the Rust crate's `temporal` cargo
feature. The engine's default build (`.../go`) depends on none of the Temporal
SDK — only a consumer that imports this module takes on `go.temporal.io/sdk`, so a
consumer that does not opt in stays zero-infra.

```go
import temporal "github.com/SmooAI/smooth-operator-core/go/temporal"
```

## What it gives you

- **Crash-safe resume** — the workflow's event history is the checkpoint, so a
  worker restart mid-turn resumes rather than restarting the turn.
- **Durable human-in-the-loop** — a tool listed in `ApprovalRequiredTools` blocks
  on `workflow.Await` until an `approve_tool` / `deny_tool` signal names its call
  id. The pending decision lives in workflow history, so it survives a
  disconnected client and can resolve hours later. A denial returns a tool-error
  result to the model without ever executing the tool.
- **Durable timers** — a model call to the configured `WaitTool` (with an integer
  `seconds` argument) sleeps the workflow on a Temporal timer, a pause that can
  span days.

## Usage

Build the activities against your model provider + tools, then register the
workflow and activities on a worker. Unlike the Rust backend (which holds engine
handles process-globally because its SDK registers activities as free functions),
the Go SDK registers activity **methods**, so the handles live on the
`AgentTurnActivities` value — no global init step:

```go
acts := temporal.NewAgentTurnActivities(myLlmProvider, "", myTools)

w := worker.New(c, "smooth-operator", worker.Options{})
w.RegisterWorkflow(temporal.AgentTurnWorkflow)
w.RegisterActivity(acts)
_ = w.Start()
```

Start a turn with `client.ExecuteWorkflow(ctx, opts, temporal.AgentTurnWorkflow,
temporal.AgentTurnInput{...})`. See `AgentTurnWorkflow` / `AgentTurnActivities`
for the workflow surface and `AgentTurnInput` for what a turn is seeded with.

## Tests

`dto_test.go` exercises the serde boundary the activities marshal across and runs
with no Temporal server. The `*_e2e_test.go` files run the workflow end to end
against a real dev server; they **self-skip** when no Temporal is reachable — set
`TEMPORAL_ADDRESS` to point at an existing server, or let the SDK download and run
an ephemeral dev server (the default, no Docker required).

## License

MIT
