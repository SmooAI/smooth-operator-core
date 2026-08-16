---
"@smooai/smooth-operator-core": patch
---

feat(dotnet): wire the SEP extension host into the agent loop

The C# engine shipped a complete SEP extension host — process, protocol, hook
fold, cross-tool guard, tool proxies — that the agent loop never called.
`SmoothAgent.cs` contained zero references to it, so every extension was
published-but-unreachable code. This is the last of the four ports to close that
gap (Go #112, Python #114, TypeScript #115); Rust reaches the same subsystem
through `Agent::with_extension_host`.

C# now has that seam, as `AgentOptions.Extensions`:

- **Tools merge.** A host's eager tools join the agent's tool set as ORDINARY
  tools (already namespaced `<extension>.<tool>`), so the model sees them, the
  loop dispatches them, and the existing permission/clearance machinery gates
  them with no special casing. Deferred tools join the `tool_search` pool and
  stay invisible until promoted.
- **`tool_call` hook.** Folded over every pending call BEFORE any of them run. A
  veto stops the call reaching dispatch and returns the reason to the model as
  `error: blocked by extension: <reason>` — wording identical across all five
  engines. A proceed-with-patch rewrites the call's arguments.
- **Turn events.** `turn_start` at the top of both `RunAsync` and
  `RunStreamingAsync`; `message_end` then `turn_end` on every exit — budget
  breach and iteration cap included, not just the happy path.

Two things differ from the Go template, both deliberate:

- The seam is a small `IExtensionHooks` interface that `ExtensionHost`
  implements. Go needs its interface to break an import cycle, which C# does not
  have; the reason here is testability — `ExtensionHost` is sealed with a private
  constructor and is only reachable via `Empty()` or a real subprocess load, so
  the loop-wiring tests need a stub. It also matches how every other seam in this
  engine is expressed (`IToolHook`, `IKnowledgeBase`, `IHumanGate`).
- The host's tools are merged into agent-private locals rather than back onto the
  passed-in options. Go's `AgentOptions` is a value copy; C#'s is a shared object,
  so appending to it would leak into the caller and double-add the extension tools
  on a second agent built from the same options. Covered by a test.

Ported the Go seam suite (visibility/dispatch, deferred stays hidden, veto with
reason, argument rewrite, event order + payloads, budget exit, streaming parity,
no-host no-op) plus the shared-options case, and mutation-checked the veto and
rewrite tests by disabling the fold.
