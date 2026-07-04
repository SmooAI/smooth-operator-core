# @smooai/smooth-operator-core

## 0.17.0

### Minor Changes

- b222cbe: SEP host: extension integrity verification + subprocess env hardening (th-210910).

  SEP extensions are spawned as subprocesses (JSON-RPC over stdio). They were
  previously launched with the host's full environment and ambient authority.
  This lands the portable, high-value subset of hardening:

  - **Integrity verification** — a second gate after the load allow-list. When a
    manifest pins `[run] sha256`, the host hashes the resolved command binary
    before spawning and refuses (both initial load and hot reload) on mismatch.
    When no pin is set, the observed hash is logged so a consumer can pin it
    (TOFU). Pinned-but-unresolvable commands are refused.
  - **Environment scrub** — the child no longer inherits the host environment.
    The spawn does `.env_clear()` and passes through only a small allow-list of
    launch essentials (`PATH`, `HOME`, locale, `TMPDIR`, `TERM`, `SystemRoot`)
    plus the manifest's explicit `[run] env`. Ambient secrets (cloud creds, API
    tokens) can no longer leak into an extension via inherited env — the
    lethal-trifecta concern.

  OS-specific sandboxing (Linux seccomp-bpf, uid/gid drop, Landlock; macOS
  `sandbox_init`) is explicitly out of scope and tracked as the next increment.

## 0.16.0

### Minor Changes

- 399ba12: th-25ce5c: Multimodal message content — carry image attachments through the conversation model and emit them as OpenAI `image_url` content parts.

  `Message` gains an `images: Vec<ImageContent>` field (a new `Message::user_with_images` constructor) that the OpenAI-compat LLM client serializes as a standard multimodal content-parts array (`[{type:text,...},{type:image_url,image_url:{url,detail}}]`) when a user message carries images. Text-only turns are byte-identical to before (`skip_serializing_if` omits the field), so no regression on non-vision chat. The prompt-cache marker path is guarded to pass image parts through untouched rather than flattening them into a text block (which would silently drop the images). Foundation for Big Smooth's vision/document support (epic th-3be564); consumed downstream by a git-rev bump.

## 0.15.0

### Minor Changes

- 666611f: Make `ToolHook::post_call` a redaction seam and have `NarcHook` redact leaked secrets.

  `post_call` now takes `&mut ToolResult` instead of `&ToolResult`, so a hook can
  rewrite a tool result's `content` in place and the mutation is what the caller —
  and therefore the LLM/conversation and every downstream consumer — actually
  sees. The default trait impl remains a no-op; `ToolRegistry::execute` and
  `execute_single` pass the result mutably through the post-hook chain.

  `NarcHook::post_call` uses the new seam: when a tool result leaks a secret it
  still raises a `Severity::Block` alert, but now also replaces the matched
  credential with `[REDACTED:<pattern-name>]` in the result content before it
  reaches the model. Clean results pass through untouched, and injection patterns
  in results remain surveillance-only (detected and alerted, not rewritten).

## 0.14.0

### Minor Changes

- 84c2fac: th-6b3ab4: route an `Ask` permission verdict to a human instead of always failing closed.

  The permission gate (th-d32ce6) blocked every `Ask` verdict, since the crate had
  no interactive approver. `PermissionHook` now accepts an optional approver over
  the same `HumanRequest`/`HumanResponse` bridge `ConfirmationHook` already uses
  (`human_channel()`):

  - **`PermissionHook::with_approver(tx, rx, timeout)`** — on an `Ask`, sends a
    `HumanRequest::Confirm` and blocks (up to `timeout`) on the response. Approve
    lets the call run; deny / timeout / dropped channel all block (fail-closed).
  - **`Agent::with_extension_host`** wires the approver automatically when a human
    channel is present (via `Agent::with_human_channel`), with a 5-minute default
    window; with no channel the hook fails closed exactly as before.
  - **A `Deny` is never routed to the human** — circuit-breakers (credential
    paths, `rm -rf /`, pipe-to-shell, env dumps, dangerous domains) stay
    non-waivable. Covered by a regression test asserting no prompt is sent.

  Persisted allow-lists (smooth's `wonk-allow.toml`, "approve and don't ask
  again") remain a follow-up — every `Ask` is currently approve-once.

## 0.13.0

### Minor Changes

- c04808a: th-5f7227: scan SEP extension tool arguments + results for secrets and prompt injection.

  The Smooth Extension Protocol host sent extension tool **arguments** to the
  subprocess unscanned and returned the subprocess's tool **result** content to
  the model verbatim — no secret-detection or prompt-injection scanning at the
  extension boundary. The just-merged `PermissionHook` (th-d32ce6) gates
  allow/ask/deny and the dangerous-command circuit-breakers, but does no content
  scanning.

  New `narc` module (`src/narc.rs`) ports smooth's `smooth-narc` surveillance
  model natively (it can't be imported — smooth depends on this crate):

  - **`NarcHook`** — a `ToolHook` installed on the extension-host `ToolRegistry`
    in `Agent::with_extension_host`, **after** the `PermissionHook` (permission
    gate first, then Narc scans the calls that clear it). Gated behind
    extension-host attachment, so non-extension agents are unaffected.
  - **Secret detection** — 10 patterns (AWS access/secret keys, Anthropic/OpenAI
    keys, GitHub tokens, private keys, generic secrets, bearer tokens, base64
    keys, Stripe keys). Matches are redacted before logging.
  - **Prompt-injection detection** — 8 patterns (instruction override, role
    hijack, system-prompt injection, jailbreak, base64 smuggling, data/URL
    exfiltration, smell URLs), each carrying a severity.
  - **`pre_call`** blocks the call (`Err`) on a `Block`-severity injection pattern
    in the arguments (active data/URL exfiltration); lower-severity injection and
    any secret in the arguments are alerted (detect + log), not blocked — a tool
    arg legitimately carrying a secret is common enough that a hard block would be
    a footgun.
  - **`post_call`** detects secrets/injection in the result and records + logs a
    severity alert, but **cannot redact** — the `post_call` seam takes an
    immutable `&ToolResult` and its `Err` is only logged by the registry.
    Redacting a leaked result requires a mutable seam change, deliberately out of
    scope here and filed as a follow-up.

  Deliberately does **not** re-port smooth-narc's `CliGuard`/`WriteGuard` — the
  `PermissionHook` already owns dangerous-command and write gating. Exhaustively
  tested (30 tests): each secret pattern positive + near-miss negative, each
  injection pattern, `pre_call` blocks on exfiltration, `post_call` detects a
  secret leak in a result, and an integration test proving the hook is live on a
  real `ToolRegistry`.

## 0.12.0

### Minor Changes

- 72c646b: th-d32ce6: gate SEP extension (and native) tool calls behind a permission classifier.

  The Smooth Extension Protocol host executed extension-contributed tools with no
  permission gate — once an extension cleared the load allowlist it ran any tool
  freely: no allow/ask/deny model, no dangerous-command classifier, no
  circuit-breakers.

  New `permission` module (`src/permission.rs`) ports the classification model
  natively from smooth's `smooth-bigsmooth::auto_mode` (it can't be imported —
  smooth depends on this crate):

  - **`decide(mode, tool_name, args) -> Verdict`** — pure, deterministic
    classifier. Read-only → Allow, mutating → Ask, dangerous → Deny.
  - **Hard circuit-breakers (deny in every mode, incl. `Bypass`)**: credential
    paths (`~/.ssh/id_*`, `~/.aws/credentials`, dotenv files, smooth's own secret
    stores), `rm -rf /` family, `curl … | sh` / pipe-to-shell (incl. `sudo bash`
    sinks), fork bombs, `mkfs`/`dd`, env-dumps (`env`/`printenv`/`$SECRET`
    echoes, `$(env)` substitution smuggling), and dangerous domains
    (pastebin/transfer.sh/ngrok/crypto). Compound commands (`ls && rm -rf /`) are
    split so a safe first command can't shield a dangerous tail.
  - **Modes via `SMOOTH_AUTO_MODE`**: `ask` (default) / `accept-edits` / `deny`
    (headless) / `bypass`.
  - **`PermissionHook`** (`ToolHook::pre_call`) blocks on Deny and — fail-closed,
    since this crate has no interactive approver — on Ask.

  Wired onto the agent's `ToolRegistry` in `Agent::with_extension_host`, gating
  every tool call. New `Agent::with_permission_mode(mode)` lets a consumer set the
  posture (before attaching the host) without the `SMOOTH_AUTO_MODE` env var.

  Secure by default: unmatched extension tools now require approval and, with no
  approver, are blocked. Consumers that trust their extensions opt into
  `AutoMode::Bypass` (hard circuit-breakers still fire).

  Interactive Ask routing (a confirm bridge so Ask can prompt a human instead of
  failing closed) is deferred to a follow-up pearl.

## 0.11.1

### Patch Changes

- aef7a89: SEP security fix (th-f0e020): scope what an extension `tool_call` **Modify** can
  do. The `tool_call` hook fires over every pending call the model made — native
  tools (`bash`, `file-write`) included — and a `Modify` verdict was applied
  verbatim as a full `{tool, arguments}` replacement with no validation. So
  enabling ANY extension let its hook silently rewrite the arguments of a bash /
  file-write call — or redirect the call to a different tool — with zero
  oversight.

  The fold driver (`ExtensionHost::run_hook`) now guards every `tool_call` Modify:

  - The `tool` field is immutable across a hook — a Modify that renames the tool
    is rejected (redirecting call A to a different tool is never legitimate).
  - An extension may only rewrite the arguments of a tool it **owns**
    (namespaced `<ext>.<tool>`). A Modify targeting a native tool or another
    extension's tool is rejected.

  Rejected Modifies are downgraded to `Continue` (the original call is preserved)
  and logged as a security warning. **Blocking is unaffected** — an extension can
  still `Block` any call, native or not; only silent mutation is scoped. Continue,
  Block, fail-closed timeout semantics, and Modify of the extension's own tool args
  are all unchanged. Exhaustive adversarial unit tests cover tool-rename,
  native-tool rewrite, foreign-extension rewrite, and the legitimate own-tool
  cases.

## 0.11.0

### Minor Changes

- ef39b43: SEP Phase 8 (engine) — long-tail pi parity:

  - **Inter-extension bus**: `bus/publish` now fans out as a `bus/event` observe
    event to every other extension subscribed to it (`BusRegistry` shares the
    loaded extensions' process + subscription handles; a `Weak` process ref avoids
    a reference cycle; a hot reload's subscription swap is reflected with no
    re-registration).
  - **`context` hook wired**: extensions can replace the entire message array the
    LLM sees each iteration (pi's `context` middleware analog) via a pi-friendly
    `{role, content}` wire shape. Zero-copy and skipped when no extension declares
    the hook (`any_hook` gate; new optional `registrations.hooks` list).
  - **`before_agent_start` hook wired**: extensions can rewrite the system prompt
    once at run start, composing with (never replacing) the resolved persona.
    Both hooks fire on the `run` and `run_with_channel` paths.
  - **Render-block v2 keybinding routing**: `ExtensionHost::dispatch_widget_key`
    targets one extension's active widget with a `widget/key` notification,
    bypassing the observe subscription filter.
  - **Declarative message renderers**: `registrations.message_renderers` (a custom
    message `tag` → render-block template) surfaced via
    `ExtensionHost::message_renderers()`; data-only, frontend renders.

## 0.10.1

### Patch Changes

- 50919e1: Build the package before packing so the published tarball actually contains
  `dist/`. The release ran `changeset publish` with no build step and the package
  had no `prepack`/`prepare` hook, so recent versions (e.g. 0.9.0) shipped without
  compiled output — every `@smooai/smooth-operator-core` import 404s. Add
  `"prepack": "pnpm run build"` so `npm publish` builds `dist/` at pack time.

## 0.10.0

### Minor Changes

- cd80532: SEP: port the ExtensionHost to the .NET engine core (`dotnet/core`).

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

## 0.9.0

### Minor Changes

- c922f7b: SEP: port the ExtensionHost to the Go engine core (th-829d9f).

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

## 0.8.0

### Minor Changes

- 75b91dc: SEP host — port the ExtensionHost to the TypeScript engine core. New
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

## 0.7.0

### Minor Changes

- e5d1068: SEP host — port the ExtensionHost to the Python engine core.

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

- 46fbbea: SEP Phase 7 (engine) — registerProvider: declarative provider registration,
  OAuth round-trips, proxied streaming, and `session/set_model`.

  Extensions can now contribute LLM providers to the host. The engine gains:

  - **Declarative provider registration** — `ProviderRegistration` (name, base_url,
    api_key_env, oauth flag, models) rides the `initialize` handshake registrations
    and `registry/update`. `ExtensionHost::providers()` surfaces the merged set so a
    host can present extension providers in its model surface.
  - **Proxied streaming** — `ExtensionLlmProvider` implements the engine's
    `LlmProvider` trait, so an extension-registered provider is a drop-in for the
    native `LlmClient` at the agent-loop seam. The host sends `provider/complete`;
    the extension streams `provider/delta` notifications (serialized `StreamEvent`s)
    keyed by a `request_id`, then replies with the final result. Deltas are routed
    by a shared `ProviderStreams` registry and terminated cleanly when the request
    resolves; ordering (deltas before the terminal `Done`) rides the process's
    single ordered reader.
  - **OAuth round-trips** — `ExtensionHost::provider_oauth_login` /
    `provider_oauth_refresh` send `provider/oauth_login` / `provider/oauth_refresh`
    to the owning extension, which drives any user interaction back over the
    existing `ui/*` surface and returns a `ProviderCredentials` bundle.
  - **`session/set_model`** — a new tier-guarded (command-tier + current-epoch)
    `HostDelegate::session_set_model`, carrying an optional `provider` and
    `thinking` level, so an extension can switch the active model to an
    extension-registered provider/model. Plus a `model_select` SEP event name.

  Additive: nothing runs unless a host attaches an `ExtensionHost`. The reference
  `sep-echo-peer` gains a `SEP_ECHO_PROVIDER` mode exercising the whole path live.

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
