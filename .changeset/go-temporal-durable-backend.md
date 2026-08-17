---
'@smooai/smooth-operator-core': patch
---

feat(go): optional Temporal-backed durable-execution backend for the Go engine (th-137b91, I parity)

The durable-execution backend previously shipped only for Rust, while the `AgentExecutor` seam it
plugs into lives in all five engines. This closes that gap for Go: a new **separate Go module**,
`github.com/SmooAI/smooth-operator-core/go/temporal`, runs an agent turn as a Temporal workflow whose
side-effects — the model call and each tool invocation — are Temporal activities. The workflow drives
the engine's deterministic `core.DriveTurn` orchestration **unchanged** (the activities delegate to
`core.InProcessActivities`, the same side-effect code the in-process path runs inline), so the durable
path and the in-process path are the same loop — no second agent loop to keep in sync.

It mirrors the Rust `smooth-operator-temporal` crate: `AgentTurnWorkflow` + `AgentTurnActivities`,
durable human-in-the-loop (an `ApprovalRequiredTools` tool blocks on `workflow.Await` until an
`approve_tool` / `deny_tool` signal names its call id; a denial returns a tool-error result without
executing the tool), and a durable-timer `WaitTool` (a `wait` call sleeps the workflow on a Temporal
timer that can span days). Being a **separate module** is the mirror of the Rust crate's `temporal`
cargo feature — the engine's default build pulls in none of the Temporal SDK and stays zero-infra;
only a consumer that imports this module takes on `go.temporal.io/sdk`.

Tests: a serde-boundary unit test that runs without Temporal, plus four e2e tests (health, real agent
turn, durable timer, HITL approve/deny) that run against a real ephemeral dev server and self-skip when
none is reachable — the Go mirror of the Rust `*_e2e.rs`. The core CI Go job now covers the new module.
