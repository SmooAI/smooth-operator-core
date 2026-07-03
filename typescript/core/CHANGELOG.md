# @smooai/smooth-operator-core

## 0.6.0

### Minor Changes

- 26b4489: SEP Phase 4 (engine) — commands, session actions, and hot reload.

  `ExtensionHost` gains the command surface and the command-tier deadlock guard:

  - **Command dispatch** — `run_command(ext, command, arguments)` sends
    `command/execute` to the owning extension with a COMMAND-tier context;
    `complete_command(...)` round-trips `command/complete` for argument
    autocomplete (best-effort — an extension without a completer yields no
    suggestions, never an error). `commands()` and `shortcuts()` surface the
    registered slash-commands and keyboard shortcuts for a frontend's palette.
  - **Session actions** — `HostDelegate` grows `session_send_message`,
    `session_send_user_message` (`deliver_as` steer/follow_up/next_turn), and
    `session_append_entry`. The headless engine has no session, so the defaults
    report `-32004 CapabilityDisabled`; frontends with a session store override
    them. Every session action is gated by `validate_command_context`: it must
    present a COMMAND-tier context whose epoch is still current, else
    `-32003 ContextViolation` — fired in `HostInbound` BEFORE the delegate runs.
  - **Hot reload** — `reload(name)` notifies the extension (`session_shutdown`
    reason `reload`), bumps the shared epoch so every context token it still holds
    is invalidated, respawns the subprocess (the generation guard discards late
    replies from the dead child), re-runs `initialize`, and notifies it again
    (`session_start` reason `reload`). The manifest's declared-events clamp is
    re-applied so a restart can never widen a project extension's subscriptions.

  New protocol types (`CommandExecuteParams/Result`, `CommandCompleteParams/Result`,
  `Completion`, `ShortcutRegistration`, `DeliverAs`, `Session*Params`), an
  `InitializeParams.flags` map for delivering parsed CLI flag values, and a
  `Registrations.shortcuts` list. The reference `sep-echo-peer` registers a command

  - shortcut and answers `command/execute`/`command/complete`. Purely additive:
    with no extension host attached the agent loop is unchanged.

## 0.5.0

### Minor Changes

- 2c3008b: SEP Phase 3 (engine) — thread `ui_capabilities` through the handshake.

  `ExtensionHost::load` now takes a `ui_capabilities: Vec<String>` and forwards it
  into each extension's `initialize` params, so a host declares which `ui/request`
  kinds its frontend can render (`select`/`confirm`/`input`/`notify`/`set_status`/
  `set_widget`/`set_title`). Extensions gate their UI on this list (the SDK's
  `hasUI`); the ext→host `ui/request` seam and its headless `-32001 NoUI` default
  already landed in Phase 2's `HostDelegate`. A new `SEP_ECHO_UI` mode on the
  reference `sep-echo-peer` round-trips a `ui/request` confirm from inside a
  `tool/execute`, echoing the negotiated caps into the prompt, exercised by the new
  `sep_ui_path` integration test (answered verdict + headless NoUI).

  The engine ships headless (empty caps); smooth-code and the daemon supply the
  real capability set and a `HostDelegate` that renders the dialogs.

## 0.4.0

### Minor Changes

- 2466187: SEP Phase 2 — the event bus + the intercept tier.

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

## 0.3.0

### Minor Changes

- ecb6487: Complete the SEP engine tool path — Phase 1.

  `Agent::with_extension_host` now registers every extension tool into the agent's
  `ToolRegistry` (eager via `register_arc`, deferred via the new `register_deferred_arc`)
  under its dotted `<extension>.<tool>` name, so extension tools are ordinary registry tools:
  visible to the LLM through `schemas()`, dispatched through `execute()`, and filtered by the
  same `retain()` a server uses to enforce a per-agent `enabled_tools` allow-list — no
  special-casing, and no widening of the allow-list.

  `tool/execute` gains full streaming + cancellation: `tool/update` progress notifications
  route through a new `HostDelegate::tool_update` seam, and a `CancelGuard` sends `$/cancel`
  (and clears the pending slot) whenever an awaiting request is dropped or times out, leaving
  the connection healthy for the next call. The `sep-echo-peer` reference peer gains a slow
  mode that streams progress then withholds its reply until cancelled, and new integration
  tests cover the LLM→extension round-trip, registry filtering, and the update/cancel wire.

## 0.2.0

### Minor Changes

- 1d5b4f6: Add the SEP (Smooth Extension Protocol) engine host — Phase 0.

  New additive `extension` module: JSON-RPC 2.0 wire types (`protocol`), `extension.toml`
  discovery/merge with `${env:VAR}` expansion (`manifest`), a subprocess with an ndjson
  codec and generation-guarded restart (`process`), the `ExtensionHost` with load-order
  hook chaining, fail-open/fail-closed hook policy, event fanout, and a headless
  `HostDelegate` (`host`), and `ExtensionTool` exposing an extension's tools as ordinary
  `Tool`s (`tool_proxy`). `Agent::with_extension_host` wires it in; new additive
  `AgentEvent` variants (`TurnStart`/`TurnEnd`/`MessageUpdate`/`MessageEnd`/`ToolCallUpdate`)
  are defined. With no host attached the agent loop is unchanged.
