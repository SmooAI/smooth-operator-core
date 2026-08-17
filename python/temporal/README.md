# smooai-smooth-operator-temporal (Python)

Optional [Temporal](https://temporal.io) durable-execution backend for the
[`smooai-smooth-operator-core`](https://pypi.org/project/smooai-smooth-operator-core/)
Python agent engine (ADR-030). The Python sibling of the
[`smooai-smooth-operator-temporal`](https://crates.io/crates/smooai-smooth-operator-temporal)
Rust crate.

An agent turn runs as a Temporal **workflow** whose side-effects — the model call
and each tool invocation — are Temporal **activities**. The workflow drives the
engine's deterministic `drive_turn` orchestration unchanged, so the durable path
and the in-process path are the *same loop*. That buys crash-safe resume, durable
human-in-the-loop, and durable timers without a second implementation of the agent
loop to keep in sync.

## Optionality

The serde DTO boundary (`smooth_operator_temporal.dto`) imports **no** Temporal
SDK and is always available. The workflow/activity wiring
(`smooth_operator_temporal.temporal`) imports `temporalio`, provided by the
optional `temporal` extra — mirroring the Rust crate's off-by-default `temporal`
cargo feature:

```sh
pip install 'smooai-smooth-operator-temporal[temporal]'
```

## What it gives you

- **Crash-safe resume** — the workflow's event history is the checkpoint, so a
  worker restart mid-turn resumes rather than restarting the turn.
- **Durable human-in-the-loop** — a tool named in `approval_required_tools` blocks
  on `workflow.wait_condition` until an `approve_tool` / `deny_tool` signal names
  its call id. The pending decision lives in workflow history, so it survives a
  disconnected client and can resolve hours later. A denial returns a tool-error
  result to the model without ever executing the tool.
- **Durable timers** — a call to the configured `wait_tool` sleeps the workflow on
  a Temporal timer, a pause that can span days.

## Usage

Register the workflow and activities on a worker, injecting the engine handles the
activities run against:

```python
from temporalio.client import Client
from temporalio.worker import Worker
from smooth_operator_temporal.temporal import AgentTurnActivities, AgentTurnWorkflow, HealthWorkflow

activities = AgentTurnActivities.from_engine(my_llm_provider)  # AgentOptions optional
client = await Client.connect("localhost:7233")
worker = Worker(
    client,
    task_queue="smooth-operator-agent-turn",
    workflows=[AgentTurnWorkflow, HealthWorkflow],
    activities=[activities.health_echo, activities.model_call, activities.tool_invoke],
)
```

Start a turn:

```python
from smooth_operator_temporal.dto import AgentTurnInput

messages = await client.execute_workflow(
    AgentTurnWorkflow.run,
    AgentTurnInput(system_prompt="You are a test agent", user_message="what is the durable answer?"),
    id="agent-turn-1",
    task_queue="smooth-operator-agent-turn",
)
```

Unlike the Rust reference — whose activities are free functions fed by a
process-global `init_engine` — Python holds the engine handles on the
`AgentTurnActivities` **instance** (plain dependency injection).

## Status

The durable **backend** (workflow + activities + durable timer + HITL signals) is
implemented and covered by skip-gated e2e tests. Adapting it behind the engine's
streaming `AgentExecutor` protocol — so a server can select it per turn — is
deferred for the same reason the Rust server defers it: a workflow-backed turn has
no token-delta stream to feed the runner's event translator. The server-side
selection *seam* (env-gated, injected) lands separately in the smooth-operator
server repo.

## License

MIT
