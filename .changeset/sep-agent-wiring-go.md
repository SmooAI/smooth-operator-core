---
"@smooai/smooth-operator-core": patch
---

feat(go): wire the SEP extension host into the agent loop

The Go engine shipped a complete SEP extension host — process, protocol, hook
fold, cross-tool guard, tool proxy — that the agent loop never called. `agent.go`
contained zero references to it, so every extension was published-but-unreachable
code. Rust reaches the same subsystem through `Agent::with_extension_host`.

Go now has that seam, as `AgentOptions.Extensions`:

- **Tools merge.** A host's eager tools join the agent's tool set as ORDINARY
  tools (already namespaced `<extension>.<tool>`), so the model sees them, the
  loop dispatches them, and the existing permission/clearance machinery gates
  them with no special casing — the same property the Rust host has. Deferred
  tools stay hidden until `tool_search` promotes them.
- **`tool_call` hook.** Folded over every pending call BEFORE any of them run. A
  veto stops the call reaching dispatch and returns the reason to the model as
  the tool result; a `Modify` rewrites the arguments the tool executes with.
  Rewrites are already scoped by the host's cross-tool guard.
- **Turn events.** `turn_start` at the top of a turn, then `message_end` and
  `turn_end` at every exit (including the budget-exceeded and max-iteration
  exits), matching Rust's ordering.

The seam is an interface rather than a concrete host type because the dependency
runs `extension` → `core` (an `ExtensionTool` is a `core.Tool`); `core` importing
back would be a cycle. `extension.NewAgentBridge(host)` supplies the
implementation and does the type translation.

`Extensions` unset leaves the loop byte-for-byte as it was — no dispatch, no hook
calls, no extra allocation.
