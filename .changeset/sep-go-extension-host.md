---
'@smooai/smooth-operator-core': minor
---

SEP: port the ExtensionHost to the Go engine core (th-829d9f).

The SEP host previously existed only in Rust. The Go engine core gains a new
`go/core/extension` package that mirrors the Rust reference semantics idiomatically:

- **Manifest discovery** — `extension.toml` discovery across the global
  (`~/.smooth/extensions`) and project (`.smooth/extensions`) dirs, project-wins
  merge, `${env:VAR}` expansion, single-malformed-manifest tolerance.
- **Subprocess spawner** — `ExtensionProcess`: JSON-RPC 2.0 ndjson over stdio
  (goroutines + channels), pending-request map, generation-guarded crash-restart
  (backoff 1s/5s/25s), bounded/lossy observe lane with an `events_lost` marker,
  best-effort `$/cancel` on timeout, ping health, graceful shutdown, child reaping.
- **Host** — `ExtensionHost`: load-order hook chaining with per-class fail
  policy (`tool_call`/`user_bash` fail-closed at 60s, others fail-open at 5s),
  non-blocking event fanout clamped to declared subscriptions, the command-tier
  epoch deadlock guard, and a `HostDelegate` seam (headless defaults: NoUI,
  JSON-file kv, exec denied, session actions disabled).
- **Tool proxies** — `ExtensionTool` structurally satisfies `core.Tool`, so a
  host's tools drop straight into `core.AgentOptions.Tools`.

Purely additive — with no host built the agent loop behaves exactly as before.
Covered by unit tests (exhaustive fold + context-guard adversarial cases),
vendored SEP conformance-fixture replay, and live subprocess tests against a
self-re-exec echo peer, all race-clean.
