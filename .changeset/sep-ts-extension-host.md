---
'@smooai/smooth-operator-core': minor
---

SEP host — port the ExtensionHost to the TypeScript engine core. New
`@smooai/smooth-operator-core/extension` subpath export mirrors the Rust
reference host: `extension.toml` discovery (global `~/.smooth/extensions` +
project `.smooth/extensions`, project-wins, `${env:VAR}` expansion), a
JSON-RPC/ndjson subprocess spawner (`ExtensionProcess`: handshake, pending map,
generation-guarded crash-restart with 1s/5s/25s backoff, ping health, bounded
lossy observe lane + `events_lost` marker), the `ExtensionHost` orchestrator
(load-order hook chaining with per-class timeouts — `tool_call`/`user_bash`
fail-CLOSED at 60s, others fail-open at 5s — event fanout, `<ext>.<tool>` tool
proxies, command/shortcut registration, hot reload), a `HostDelegate` seam
(ui/kv/exec/session, headless defaults) and the command-tier + epoch context
guard for session actions. Purely additive: nothing runs until a caller builds
an `ExtensionHost` and registers its tools.
