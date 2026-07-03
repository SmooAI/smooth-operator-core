---
'@smooai/smooth-operator-core': minor
---

SEP host — port the ExtensionHost to the Python engine core.

The Smooth Extension Protocol host existed only in Rust; the Python engine now has
a faithful asyncio sibling under `smooth_operator_core.extension`, so a Python host
(the operator server, the daemon) can host `extension.toml` extensions. Purely
additive — nothing runs unless a caller builds an `ExtensionHost`.

- **protocol** — JSON-RPC 2.0 ndjson frames + typed method params/results
  (`Message`, `HookOutcome`, `InitializeParams/Result`, `ToolExecuteParams/Result`,
  `EventParams`, …). Replays the shared `spec/extension/conformance/fixtures.json`
  green (round-trips valid instances, rejects the `$invalid` set).
- **manifest** — `extension.toml` discovery, global (`~/.smooth/extensions`) +
  project (`.smooth/extensions`) merge with project-wins, and `${env:VAR}` expansion.
- **process** — one subprocess per extension: asyncio ndjson codec, pending-futures
  map, generation-guarded in-place restart, a reliable control lane over a bounded,
  lossy observe lane (sheds oldest + emits an out-of-band `events_lost` marker),
  `$/cancel` on timeout/cancellation, and `ping` health.
- **host** — hook chaining in load order (`fold_hook_chain`: continue/modify/block,
  per-class timeouts — `tool_call`/`user_bash` 60s fail-CLOSED, others 5s fail-open),
  non-blocking event fanout, ext-tool proxies (`ExtensionTool`, dotted
  `<ext>.<tool>`), the `HostDelegate` seam (headless defaults: NoUI, JSON-file kv,
  exec denied, session actions unavailable), and the command-tier + epoch deadlock
  guard for session-mutating ext→host actions.

Exhaustively unit-tested (fold policy, context guard, delegate defaults), plus a
live-subprocess suite and an integration test driving a real echo peer through the
host (tool proxy + `enabled_tools` filtering parity).
