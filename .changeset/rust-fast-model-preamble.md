---
"@smooai/smooth-operator-core": patch
---

feat(rust): optional fast-model preamble to cover a reasoning model's time-to-first-token

A reasoning model behind the gateway spends its whole time-to-first-token on
reasoning plus the first tool call, so the user watches dead air before any
token arrives. `AgentConfig::with_preamble` opts a host into covering that gap:
on the first turn the agent fires a small fast model *in parallel* with the main
model and emits one short present-tense sentence describing what it is about to
do, as a new `AgentEvent::PreambleDelta`.

The event is deliberately distinct from `TokenDelta` so consumers render it as
an *ephemeral* status line that the real answer replaces, never as permanent
chat content. Older consumers skip the unknown variant and simply show no
preamble.

Off unless `preamble` is set — no extra LLM call and no behavior change for
existing consumers. The preamble task is best-effort: it runs on its own task
and any failure or slowness is swallowed, so it can never block or break the
real turn. In production it builds a dedicated client on the fast model sharing
the main model's gateway, key and request metadata, so its spend is attributed
like any other call.

Rust reference implementation only; the TS/Go/.NET/Python ports skip it until a
non-Rust consumer needs it.
