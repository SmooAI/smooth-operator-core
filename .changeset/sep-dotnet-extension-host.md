---
'@smooai/smooth-operator-core': minor
---

SEP: port the ExtensionHost to the .NET engine core (`dotnet/core`).

The Smooth Extension Protocol host existed only in Rust. This ports it to C#,
idiomatic Microsoft.Extensions.AI, under `SmooAI.SmoothOperator.Core.Extensions`:

- **Manifest discovery** — `ExtensionManifest` / `ExtensionDiscovery` parse
  `extension.toml` (Tomlyn), discover global (`~/.smooth/extensions`) + project
  (`.smooth/extensions`) extensions with project-wins merge, `${env:VAR}`
  expansion, and single-bad-manifest tolerance.
- **Subprocess spawner** — `ExtensionProcess`: JSON-RPC 2.0 / ndjson over a child's
  stdio, a pending `TaskCompletionSource` map, a generation guard + crash-restart
  backoff (1s/5s/25s), `ping` health, a bounded oldest-shedding observe lane with
  an out-of-band `events_lost` marker, and `$/cancel` on timeout/cancellation.
- **Protocol** — `ExtensionProtocol`: the JSON-RPC envelope + typed method
  params/results, the tagged `HookOutcome`, and snake_case wire serialization. The
  vendored `spec/extension/conformance/fixtures.json` replays green against the
  C# types.
- **Host** — `ExtensionHost`: discover → spawn → `initialize`, load-order hook
  chaining (`tool_call`/`user_bash` fail-closed at 60s, others fail-open at 5s),
  non-blocking event fanout, tool proxies, command dispatch/completion, hot
  reload, and the `HostDelegate` ext→host seam (ui/kv/exec/session) with a
  command-tier + epoch deadlock guard. Headless `DefaultHostDelegate` defaults.
- **Tool proxy** — `ExtensionTool` is an `AIFunction`, so an extension's tools
  drop straight into `AgentOptions.Tools` and the engine's agentic loop calls them
  like any native tool.

Additive: nothing runs unless a caller builds an `ExtensionHost`. Exhaustive unit
tests for the fold, the command-tier guard, discovery, and the observe lane, plus
live subprocess tests over a Node echo peer (handshake, tool round-trip, veto,
`tool_result` patch, fail-closed timeout, the `ui/request` seam, commands).
