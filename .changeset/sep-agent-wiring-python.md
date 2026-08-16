---
"@smooai/smooth-operator-core": patch
---

feat(python): wire the SEP extension host into the agent loop

The Python engine shipped a complete SEP extension host that the agent loop never
called — `agent.py` had zero references to it, so every extension was
published-but-unreachable code. Rust reaches the same subsystem through
`Agent::with_extension_host`.

Python now has that seam as `AgentOptions.extensions`, taking the host directly
(unlike Go, which needed an interface to dodge an import cycle):

- **Tools merge.** A host's eager tools join the agent's tool set as ORDINARY
  tools (already namespaced `<extension>.<tool>`), so the model sees them, the
  loop dispatches them, and the existing permission/clearance machinery gates
  them with no special casing. Deferred tools stay hidden — and undispatchable —
  until `tool_search` promotes them.
- **`tool_call` hook.** Folded over every pending call BEFORE any of them run. A
  veto stops the call reaching `_dispatch_tool` and returns its reason to the
  model as the tool result; a `Modify` rewrites the arguments the tool executes
  with. Rewrites are already scoped by the host's cross-tool guard.
- **Turn events.** `turn_start` at the top, then `message_end` and `turn_end` at
  every exit — including the budget-exceeded and max-iteration exits — matching
  Rust's ordering.

`extensions` unset leaves the loop exactly as it was: no dispatch, no hook calls.
