---
"@smooai/smooth-operator-core": minor
---

SEP Phase 4 (engine) — commands, session actions, and hot reload.

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
+ shortcut and answers `command/execute`/`command/complete`. Purely additive:
with no extension host attached the agent loop is unchanged.
