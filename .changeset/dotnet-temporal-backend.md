---
'@smooai/smooth-operator-core': minor
---

Add the .NET Temporal-backed durable execution backend (ADR-030), the C# sibling of the Rust
`smooth-operator-temporal` crate. The new, optional `SmooAI.SmoothOperator.Temporal` package runs an
agent turn as a Temporal workflow (`AgentTurnWorkflow`) whose model call and each tool invocation are
Temporal activities, driving the engine's `AgentTurn.DriveTurnAsync` orchestration unchanged — one
loop, two backends. It ships crash-safe resume, durable human-in-the-loop via `ApproveTool` /
`DenyTool` signals, and a durable-wait timer, plus a `TemporalExecutor : IAgentExecutor` that swaps in
behind the executor seam. The `Temporalio` SDK lives only in this separate package, so the published
core stays zero-infra.

This closes the last language gap in the parity docs' one honest exception — the durable-execution
backend now ships for Rust **and** .NET, not Rust alone (the `AgentExecutor` seam was already in all
five). Also removes `ConfigureAwait(false)` from `AgentTurn.DriveTurnAsync`, which its own contract
promised is valid Temporal workflow code: inside the workflow scheduler that call posted the
continuation off the single-threaded scheduler and hung the turn; in-process it has no captured
context to marshal back to, so dropping it is behavior-neutral there.
