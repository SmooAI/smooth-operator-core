---
'@smooai/smooth-operator-core': minor
---

Fix a permission bypass on the .NET durable execution path, and stop it dropping thread history.

**The bypass.** `InProcessActivities.ToolInvokeAsync` and the Temporal worker's
`AgentTurnActivities.ToolInvoke` both resolved a tool by name and called `AIFunction.InvokeAsync`
directly. Everything under `Permission/` — the circuit breakers, the consumer `DenyPolicy`, grants,
auto-mode, the human-approval gate, the tool hooks and the SEP `tool_call` extension fold — lives in
`SmoothAgent.InvokeToolAsync`, which neither path went through. So a consumer running
`PermissionMode = DenyUnmatched` behind a production deny policy lost the entire gate the moment they
swapped `InProcessExecutor` for `TemporalExecutor`: `bash("rm -rf /")` executed. That is the exact
seam ADR-030 exists to make behaviorally equivalent, and .NET was the only engine that regressed —
Rust routes through `ToolRegistry::execute`, Python through `SmoothAgent._dispatch_tool_result`.

Both surfaces now dispatch through the new public `SmoothAgent.DispatchToolAsync`, the single gated
entry point, mirroring how Python's activities hold a `SmoothAgent` purely as the dispatch surface.
`InProcessActivities` and `EngineHandles` each take that agent (their old model-client + tool-set
constructors still work and behave exactly as before — nothing configured, nothing gated).

**The dropped thread.** `TemporalExecutor.BuildInput` accepted a `SmoothAgentThread` and never read
it, and never appended the result back, so every durable turn started from a blank conversation while
its doc-comment claimed equivalence with `RunAsync`. `AgentTurnInput` now carries `History`, seeds it
between the system prompt and the user message, and the executor appends the turn's new messages onto
the thread. `TurnConversation` carries `ChatMessageDto` instead of a flattened role+text `TurnMessage`
(now removed) — a lossy round trip would have replayed an assistant turn stripped of its tool calls
and a tool result stripped of its call id, which the *next* turn's model rejects.

Not fixed here: `AgentOptions.Budget` is still unenforced on the durable path, because
`AgentTurn.DriveTurnAsync` has no spend concept in any language.

Regression tests live where CI runs them (`dotnet/core/tests`, `dotnet/temporal/tests`), not behind
the `SMOOTH_AGENT_TEMPORAL_E2E` gate that .NET CI never sets. Five of them fail against the old
dispatch; the deny-policy tests build their call through the serde DTO boundary, so the gate is proven
to still match once arguments arrive as `JsonElement`s off the wire.
