---
"@smooai/smooth-operator-core": minor
---

SEP Phase 2 — the event bus + the intercept tier.

**Observe events** now fan out end-to-end. `dispatch_event` routes through a new
per-connection bounded observe lane in `ExtensionProcess`: events carry a
monotonic `seq`, and when a slow/stalled extension lets the queue pass 1024 the
oldest events are shed (never requests) and an out-of-band `events_lost` marker
(carrying the shed count) is delivered on recovery — bounded memory instead of
unbounded growth or a stalled turn. Effective subscriptions are the extension's
handshake list clamped to its manifest `[capabilities] events`. Wire event names
mirror pi's (`turn_start`/`turn_end`, `tool_execution_start`/`update`/`end`,
`message_end`) for near-mechanical porting; `model_select` maps to the existing
`AgentEvent::ModelResolved`.

**Intercept tier**: the fail-closed `tool_call` hook now applies `Modify` (arg
rewrite), not just `Block`, before execution; the new fail-open `tool_result`
hook patches a result before it is pushed to the conversation. Both hooks — and
the turn/tool events — are wired into a shared `sep_run_tool_calls` used by BOTH
`run()` and the streaming `run_with_channel()` (the path the polyglot servers and
the TUI drive), so hooks fire identically on both. A hung hook still times out
per-class, `$/cancel`s, and (for `tool_call`) fail-closed BLOCKS without stalling
the turn — covered by a new integration test with a hanging peer, plus tests for
`tool_result` patching and the observe-lane shedding. `EventParams` gains `seq`.

Zero behavior change when no `ExtensionHost` is attached (the default).

`before_agent_start` run-loop wiring is deferred to a later phase (the host method
exists and is tested; the engine's system prompt is baked at `resume_or_new` and
composing it is a frontend/server concern) — see the SEP pearls.
