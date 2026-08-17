# @smooai/smooth-operator-core

## 1.7.11

### Patch Changes

- 70387a7: feat(rust): vector store + lexical reranker, so the Rust reference stops being the one engine without them

  The inverted parity gap: Go, TypeScript, Python and .NET all shipped a vector
  store and a reranker, while the Rust reference — the engine every SmooAI
  production brain actually runs — had neither. `docs/Polyglot-Engines.md`
  nonetheless listed "Rerank — lexical reranker built in" as a shared feature, with
  an asterisk saying Rust delegates it to its host. Rust now has both, and the
  asterisk is gone.

  - **`vector::VectorKnowledge`** — an embedding-backed `KnowledgeBase` with cosine
    retrieval, alongside the existing lexical `InMemoryKnowledge`. The `Embedder`
    seam is pluggable; the default `HashEmbedder` is the ports' deterministic
    offline feature-hashing embedder (FNV-1a token hash → signed buckets →
    L2-normalize), so it needs no network and no credentials. It reuses the crate's
    existing chunker on ingest, so a `VectorKnowledge` is a drop-in swap for
    `InMemoryKnowledge`.
  - **`rerank::Reranker`** — `NoopReranker` (passthrough) and `LexicalReranker`,
    scoring `coverage / log2(2 + doc_token_count)` exactly as the ports do, so a
    concise on-topic doc outranks a long one with the same raw overlap. Stable for
    ties, so equal scores keep the retriever's order.
  - **Wired into the agent**, not left as library furniture:
    `AgentConfig::with_reranker(reranker, candidate_k)` pulls a candidate pool from
    the retriever, reranks it, and truncates to `KNOWLEDGE_TOP_K` before injecting
    the `[Relevant knowledge]` block. Unset = the previous behavior byte for byte.

  Parity was checked against Go on a shared corpus, not asserted: the same four
  documents and three queries produce identical cosine scores to six decimals in
  both engines (`0.129099`, `0.109109`, `0.176777`, …) and the reranker returns the
  identical order, ties included.

## 1.7.10

### Patch Changes

- d5a7381: fix(go,ts,python,dotnet): read the gateway's cost header so `costUsd` is real, not 0

  The LLM gateway reports per-request cost ONLY in a response header. Rust reads it
  (core#102); the four ports never did, so every turn fell through to local
  `ModelPricing` — which prices aliased `smooth-*` routes at $0. The lockstep
  version bump then shipped that as "fixed" in all five engines, which is the root
  of the costUsd-only-on-Rust finding.

  All four ports now mirror Rust's parser exactly, same candidate list and
  precedence: `x-litellm-response-cost-margin-amount` → `-original` →
  `x-litellm-response-cost` → `x-response-cost` → `x-cost-usd`, taking the FIRST
  NON-ZERO value.

  **Absent AND zero both mean "unmeasured".** A present `0` is not locked in either
  — it falls through to the next candidate, and if nothing measures, the parser
  returns null so the caller uses the local estimate. That is the actual bug: a
  real $0 and "the gateway didn't tell us" must stay distinct.

  - **Go** — full port: it owns its HTTP client, so both paths read real headers.
    Non-streaming attaches `ChatResponse.GatewayCostUSD`; streaming reads headers
    BEFORE the SSE body is consumed (they are gone once it is) and carries the cost
    on the first `ChatChunk`. `CostTracker.RecordWithGatewayCost` prefers it.
  - **TypeScript / Python / .NET** — these take an injected client (`openai` SDK,
    `IChatClient`) and have no HTTP client of their own, so they get the parser plus
    the seam the cost flows through: a `gatewayCostUsd` a wrapping client attached,
    or raw `headers` hung off the response (`.withResponse()` /
    `.with_raw_response` / `AdditionalProperties`). A client that surfaces headers
    now lands a real cost on the turn, and a native HTTP client will inherit it.

## 1.7.9

### Patch Changes

- 1a0e1af: feat(dotnet): wire the SEP extension host into the agent loop

  The C# engine shipped a complete SEP extension host — process, protocol, hook
  fold, cross-tool guard, tool proxies — that the agent loop never called.
  `SmoothAgent.cs` contained zero references to it, so every extension was
  published-but-unreachable code. This is the last of the four ports to close that
  gap (Go #112, Python #114, TypeScript #115); Rust reaches the same subsystem
  through `Agent::with_extension_host`.

  C# now has that seam, as `AgentOptions.Extensions`:

  - **Tools merge.** A host's eager tools join the agent's tool set as ORDINARY
    tools (already namespaced `<extension>.<tool>`), so the model sees them, the
    loop dispatches them, and the existing permission/clearance machinery gates
    them with no special casing. Deferred tools join the `tool_search` pool and
    stay invisible until promoted.
  - **`tool_call` hook.** Folded over every pending call BEFORE any of them run. A
    veto stops the call reaching dispatch and returns the reason to the model as
    `error: blocked by extension: <reason>` — wording identical across all five
    engines. A proceed-with-patch rewrites the call's arguments.
  - **Turn events.** `turn_start` at the top of both `RunAsync` and
    `RunStreamingAsync`; `message_end` then `turn_end` on every exit — budget
    breach and iteration cap included, not just the happy path.

  Two things differ from the Go template, both deliberate:

  - The seam is a small `IExtensionHooks` interface that `ExtensionHost`
    implements. Go needs its interface to break an import cycle, which C# does not
    have; the reason here is testability — `ExtensionHost` is sealed with a private
    constructor and is only reachable via `Empty()` or a real subprocess load, so
    the loop-wiring tests need a stub. It also matches how every other seam in this
    engine is expressed (`IToolHook`, `IKnowledgeBase`, `IHumanGate`).
  - The host's tools are merged into agent-private locals rather than back onto the
    passed-in options. Go's `AgentOptions` is a value copy; C#'s is a shared object,
    so appending to it would leak into the caller and double-add the extension tools
    on a second agent built from the same options. Covered by a test.

  Ported the Go seam suite (visibility/dispatch, deferred stays hidden, veto with
  reason, argument rewrite, event order + payloads, budget exit, streaming parity,
  no-host no-op) plus the shared-options case, and mutation-checked the veto and
  rewrite tests by disabling the fold.

## 1.7.8

### Patch Changes

- 338b7a0: TypeScript engine: wire the SEP extension host into the agent loop (`AgentOptions.extensions`) — the TS sibling of Rust's `Agent::with_extension_host` and the Go seam from #112. Eager extension tools merge in as ordinary (visible, dispatched, permission-gated) tools; deferred ones stay hidden until `tool_search` promotes them; the `tool_call` hook folds over every pending call before dispatch (veto reasons surface to the model, argument rewrites apply); and `turn_start` / `message_end` / `turn_end` events fan out on every exit — budget-exceeded and max-iteration included — in BOTH `run` and `runStream`. The seam is structural (`ExtensionHooks`), so the concrete `ExtensionHost` satisfies it with no import cycle, pinned by a compile-time assertion. No host configured ⇒ byte-identical behavior.

## 1.7.7

### Patch Changes

- da8b662: feat(python): wire the SEP extension host into the agent loop

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

## 1.7.6

### Patch Changes

- 93fce6c: feat(go): wire the SEP extension host into the agent loop

  The Go engine shipped a complete SEP extension host — process, protocol, hook
  fold, cross-tool guard, tool proxy — that the agent loop never called. `agent.go`
  contained zero references to it, so every extension was published-but-unreachable
  code. Rust reaches the same subsystem through `Agent::with_extension_host`.

  Go now has that seam, as `AgentOptions.Extensions`:

  - **Tools merge.** A host's eager tools join the agent's tool set as ORDINARY
    tools (already namespaced `<extension>.<tool>`), so the model sees them, the
    loop dispatches them, and the existing permission/clearance machinery gates
    them with no special casing — the same property the Rust host has. Deferred
    tools stay hidden until `tool_search` promotes them.
  - **`tool_call` hook.** Folded over every pending call BEFORE any of them run. A
    veto stops the call reaching dispatch and returns the reason to the model as
    the tool result; a `Modify` rewrites the arguments the tool executes with.
    Rewrites are already scoped by the host's cross-tool guard.
  - **Turn events.** `turn_start` at the top of a turn, then `message_end` and
    `turn_end` at every exit (including the budget-exceeded and max-iteration
    exits), matching Rust's ordering.

  The seam is an interface rather than a concrete host type because the dependency
  runs `extension` → `core` (an `ExtensionTool` is a `core.Tool`); `core` importing
  back would be a cycle. `extension.NewAgentBridge(host)` supplies the
  implementation and does the type translation.

  `Extensions` unset leaves the loop byte-for-byte as it was — no dispatch, no hook
  calls, no extra allocation.

## 1.7.5

### Patch Changes

- f7f041f: Polyglot parity: LLM request metadata + structured tool details for the Go, TypeScript, Python, and .NET engines.

  - **Request metadata** (Rust `ChatRequest.metadata` / `with_metadata` parity): each engine's agent options gain a `metadata` map forwarded verbatim as the OpenAI-compatible request's top-level `metadata` field on every model call — LiteLLM records it on spend logs, so hosts get per-agent LLM cost attribution at the gateway. Unset and empty are byte-identical on the wire (nothing is sent).
  - **Structured tool details** (Rust `AgentEvent::ToolCallComplete.details` parity): a post-call tool hook may attach a structured, UI-facing payload to the mutable `ToolResult`; the streaming tool-result event now forwards it verbatim and un-truncated (Go `StreamEvent.Details`, TS `tool_result.details`, Python `ToolResultEvent.details`). The model only ever sees the text content. In .NET the seam already exists via `AIContent.AdditionalProperties` on the mutable `FunctionResultContent` — now covered by a test documenting it.

## 1.7.4

### Patch Changes

- 0b449cd: feat(rust): optional fast-model preamble to cover a reasoning model's time-to-first-token

  A reasoning model behind the gateway spends its whole time-to-first-token on
  reasoning plus the first tool call, so the user watches dead air before any
  token arrives. `AgentConfig::with_preamble` opts a host into covering that gap:
  on the first turn the agent fires a small fast model _in parallel_ with the main
  model and emits one short present-tense sentence describing what it is about to
  do, as a new `AgentEvent::PreambleDelta`.

  The event is deliberately distinct from `TokenDelta` so consumers render it as
  an _ephemeral_ status line that the real answer replaces, never as permanent
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

## 1.7.3

### Patch Changes

- 41c7904: fix(go,ts,python,dotnet): SEP security parity — scrub the extension subprocess env, and guard cross-tool `tool_call` Modify

  Two security behaviors the Rust reference host has had since `th-210910` /
  `th-f0e020` were missing from all four ports, so a repo whose pitch is
  "security-first design" shipped four engines that were not.

  **Env scrubbing on extension spawn.** Go, TypeScript, Python and .NET all handed
  an extension subprocess the host's FULL ambient environment (`os.Environ()`,
  `{...process.env}`, `{**os.environ}`, and .NET's pre-populated
  `ProcessStartInfo.Environment`). Any third-party extension therefore inherited
  `AWS_SECRET_ACCESS_KEY`, `GITHUB_TOKEN` and every other ambient credential —
  the lethal-trifecta concern. Each port now mirrors Rust's `ENV_PASSTHROUGH`
  allow-list (`PATH`, `HOME`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TMPDIR`, `TERM`,
  `SystemRoot` — launch essentials, none secret) via a pure, injectable
  `build_child_env` and clears everything else. The manifest's `[run] env` is
  overlaid on top, so an extension can still _set_ — but never silently
  _inherit_ — a var.

  **Cross-tool `tool_call` Modify guard.** The `tool_call` hook fires over EVERY
  pending call the model made, native `bash`/`file-write` included, and the ports
  applied a `Modify` verdict verbatim as a full `{tool, arguments}` replacement.
  Enabling any extension therefore let its hook rewrite a pending native `bash`
  call's arguments, or redirect a call to a different tool, with zero oversight.
  Each port now runs Rust's `guard_tool_call_modify`: a `Modify` is honored only
  when it does not change which tool runs AND the acting extension owns the tool
  (`<ext>.<tool>`); otherwise it is downgraded to `Continue`, preserving the
  ORIGINAL call, and logged. `Continue`/`Block` are untouched — an extension
  blocking any call stays safe and useful; only MUTATION is scoped.

  Rust's adversarial tests are ported to each language (native-tool rewrite,
  foreign-extension rewrite, tool redirect, dot-boundary ownership, ambient-secret
  scrub, manifest-env precedence).

## 1.7.2

### Patch Changes

- 350532d: fix(rust): read `x-litellm-response-cost` on the STREAMING paths so `cost_usd` is real

  The LLM gateway (LiteLLM at llm.smoo.ai) reports per-request cost only in a
  response **header** — never in the JSON body's `usage` object, and
  `_hidden_params` is null. The non-streaming `chat()` / Anthropic-native paths
  already parsed it via `parse_gateway_cost`, but `chat_stream()` and
  `chat_anthropic_stream()` went straight from the response to `bytes_stream()`,
  dropping the headers on the floor. Since every real agent turn streams, this
  pinned `LlmResponse.gateway_cost_usd` at `None`, made the agent fall back to
  local `ModelPricing` (which prices aliased `smooth-*` routes at $0), and that
  zero propagated cleanly through `AgentEvent::Completed.cost_usd` → `TurnUsage`
  → `usage.costUsd` → the bench leaderboard, `th code`'s status bar and the
  daemon spend ledger.

  Both streaming paths now parse the cost header **before** consuming the body
  (the only point at which headers are still readable) and emit it as a new
  `StreamEvent::Cost { usd }` — the first event of the stream —
  which `accumulate_stream_events` folds onto the response. Absence is tolerated:
  no header leaves `gateway_cost_usd` at `None` and the local `ModelPricing`
  fallback is unchanged, so nothing locks in a bogus zero. Header precedence is
  shared with the non-streaming paths (`-margin-amount` beats `-original` beats
  the legacy `x-litellm-response-cost`).

## 1.7.1

### Patch Changes

- 5a560e6: feat(ts): ToolHook lifecycle for tool-call surveillance + redaction (polyglot parity with the Rust engine)

  The TypeScript engine gains the Rust `ToolHook` lifecycle. New exported types
  `ToolCall`, `ToolResult`, and `ToolHook` (with async `preCall(call)` — throw to
  block — and `postCall(call, result)` where `result` is mutable so a hook can
  redact `result.content` in place). `SmoothAgent` accepts hooks via
  `AgentOptions.toolHooks` and a new `addHook(hook)` method (the registry seam),
  running every hook's `preCall` before a tool executes and `postCall` after — the
  mutation reaches the model/conversation. A throwing `postCall` is swallowed
  (logged, not surfaced) so the redaction seam can never break a turn. Applied on
  both `run` and `runStream`. This is the seam Narc / host-supplied surveillance
  plug into, mirroring `smooth-operator-core/src/tool.rs`.

## 1.7.0

### Minor Changes

- fe882c3: First lockstep polyglot release. Changesets now drives publishing for every
  language artifact (npm + crates.io + NuGet + PyPI + Go tag) at a single shared
  version via `scripts/ci-publish.mjs`, with `scripts/sync-versions.mjs`
  propagating the Changeset version to all manifests. This aligns the previously
  divergent per-language versions (npm 0.22, Rust 0.16, .NET 1.6, Python 1.3) onto
  one lockstep line at 1.7.0 — no registry downgrades.

### Patch Changes

- fe882c3: Release infra: Changesets now drives lockstep publishing of every polyglot artifact (npm + crates.io + NuGet + PyPI + Go tag) from a single canonical version. Adds `scripts/sync-versions.mjs` (propagates the npm version to Rust/.NET/Python/Go manifests) and `scripts/ci-publish.mjs` (idempotent, skip-if-already-published, DRY_RUN) wired into `release.yml`. The per-language `publish-*.yml` workflows remain as manual fallbacks.
- fe882c3: docs: rewrite the root + per-language package READMEs as registry landing pages that tell a story

  Every README (root and the Rust / TypeScript / Python / Go / .NET package pages)
  now opens with a hook and a narrative arc — problem → one engine in five
  languages → observe→think→act → the permission gate + deny-policy that makes an
  agent safe to point at production → build → get started. Each package page leads
  with a tight agent-plus-tool quickstart in its own idiom (the mock scripted to
  call the tool, then answer) and a permissions/deny-policy example using that
  language's real API (`with_deny_policy` in Rust, `denyPolicy`/`permissionMode`
  options in TS/Py, `WithDenyPolicy` in Go/C#).

  Adds the headline permission system + deny-policy (AutoMode ask / accept-edits /
  deny-unmatched / bypass, circuit-breakers, declarative TOML rules + semantic
  predicates) to the feature surface, refreshes the polyglot table
  (language → package → registry), and fixes stale test-count claims. Docs only —
  no code changes.

## 0.23.0

### Minor Changes

- f4ba064: feat(python): permission engine + consumer deny policy (parity with the Rust reference)

  Python port of the Rust engine's tool-call permission system and the new deny
  policy (pearl th-ab0437; mirrors `permission.rs`, `permission_grants.rs`,
  `deny_policy.rs`). Three new modules, all built on the existing `ToolHook` seam:

  - **`permission`** — `AutoMode` (Ask / AcceptEdits / DenyUnmatched / Bypass, with
    `SMOOTH_AUTO_MODE` parsing), the `Verdict` union (Allow / Deny / Ask), and the
    pure `decide(mode, tool_name, args)` classifier faithfully reproducing every
    circuit-breaker: dangerous-CLI substrings, structural `curl … | sh` (across the
    pipe, sudo/wrapper-aware), credential/dotenv paths, process-env dumps
    (`env`/`printenv`/`$SECRET` expansions, command-substitution-proof), dangerous
    domains, `split_compound` / `strip_wrappers_and_sudo`, and the safe read-only
    bash/git allow-set. `PermissionHook` (a `ToolHook`) enforces it: `pre_call`
    raises on Deny; an Ask consults stored grants then routes to a `HumanGate`
    approver (fail-closed on timeout / no approver).
  - **`permission_grants`** — the `wonk-allow.toml` allow-list (`PermissionGrants`,
    `NetworkGrant`/`ToolGrant`/`BashGrant`, `SharedGrants`, atomic
    `append_grant`, layered user+project load). A grant can only upgrade an Ask,
    never waive a Deny.
  - **`deny_policy`** — `DenyPolicy` = declarative `DenyRules` (TOML: `[tools]` /
    `[bash]` / `[network]` / `[paths]` deny lists, same section style as grants) +
    a `DenyPredicate` ABC for semantic checks. Evaluated **first** in `pre_call`, so
    a policy match is a circuit-breaker no grant waives and no mode downgrades.

  Wired into `AgentOptions` via `permission_mode` + `deny_policy` — when either is
  set a `PermissionHook` is prepended so it gates every call first (a `deny_policy`
  alone activates a Bypass-mode gate: built-in breakers + policy only). Purely
  additive: with neither set, enforcement is byte-identical to before.
  `HumanDecision` gains `APPROVED_ALWAYS` (persist a grant). Adversarial tests
  ported from the Rust suites (sudo/compound/wrapper bash, network suffix+glob,
  path R/W, predicate some/none, deny-beats-grant, survives-Bypass, TOML
  round-trip).

## 0.22.0

### Minor Changes

- d85a958: Port the permission system + deny-policy to the TypeScript engine, to parity with the Rust reference (pearl th-ab0437).

  Adds a native tool-call permission gate mirroring `rust/smooth-operator-core`:

  - **`AutoMode`** (`Ask` / `AcceptEdits` / `DenyUnmatched` / `Bypass`, plus `autoModeFromEnv`/`autoModeFromValue` reading `SMOOTH_AUTO_MODE`) and **`Verdict`** (an `allow`/`deny`/`ask` discriminated union).
  - **`decide(mode, toolName, args)`** — the pure, deterministic classifier with all circuit-breakers faithfully reproduced (dangerous-CLI substrings, `curl | sh` pipe-to-shell, credential/dotenv path guard, env-dump guard, dangerous domains, compound-command splitting, `sudo`/wrapper stripping, safe read-only bash allow-list). Denies survive every mode, including `Bypass`.
  - **`PermissionGrants`** — the allow-only grant store (`network`/`tools`/`bash` sections, TOML round-trip) that can upgrade an `Ask`, never waive a `Deny`.
  - **`DenyPolicy`** — consumer-supplied declarative deny rules (`[tools]`/`[bash]`/`[network]`/`[paths]`, TOML) plus a `DenyPredicate` callback for semantic checks. Evaluated FIRST as a circuit-breaker tier: no grant waives it and no mode downgrades it.
  - **`PermissionHook`** (implements the new `ToolHook` interface) wiring it together, with `Ask` routed to the existing `HumanGate` (new `approveAlways()` / `remember` for persistent grants) and failing closed when no approver is wired.

  Wired into `SmoothAgent` via new options `permissionMode`, `denyPolicy`, and `permissionGrants`. Purely additive: with none set the gate is off and behaviour is unchanged.

## 0.21.0

### Minor Changes

- 2051413: feat(rust): consumer-supplied deny policy for the permission engine (reference impl)

  Adds a new `deny_policy` module to the Rust engine — a consumer-declarable deny
  tier that the hardcoded circuit-breakers and allow-only grants could not express
  ("never the prod AWS profile", "deny the DB writer endpoint, reads go to the
  replica", "no writes under `/prod`").

  Two tiers, both circuit-breaker strength:

  - **Declarative** `DenyRules` (serde/TOML, mirroring `permission_grants`'
    section style): `[tools] deny` (name globs), `[bash] deny_patterns` (compound-
    and sudo/wrapper-aware command prefixes/globs), `[network] deny_hosts` (suffix
    - `*.`/mid-string globs, reusing `domain_matches_suffix_list`), `[paths] deny`
      (path globs for Write/Read tools).
  - **Predicate** `DenyPredicate` trait — boxed consumer checks for semantic cases
    the engine can't parse from strings (is this the prod account? the writer
    endpoint?).

  Assembled into `DenyPolicy { declarative, predicates }` (`from_toml` + a builder
  for predicates). Wired via `PermissionHook::with_deny_policy(...)` and
  `Agent::with_deny_policy(...)`; evaluated **first** in `pre_call`, so a policy
  match is a terminal deny that no stored grant can waive and that
  `Bypass`/`AcceptEdits` cannot downgrade — the same tier as the built-in
  breakers.

  Purely additive: with no policy set, enforcement is byte-identical to before
  (proven by test). This is the reference implementation the C#/TS/Python/Go ports
  will mirror.

## 0.20.4

### Patch Changes

- 4755fcc: feat(dotnet): clamp `max_tokens` to the model's output ceiling (.NET parity)

  `AgentOptions` gains `MaxOutputTokens` (the budget), `ModelMaxOutputTokens` (the
  model's hard output ceiling), and `EffectiveMaxTokens` = `min(budget, ceiling)`
  (never 0; `null` budget = leave `max_tokens` unset; `null`/≤0 ceiling = graceful
  passthrough). `SmoothAgent` now sends the clamped value as the request's
  `ChatOptions.MaxOutputTokens`. Mirrors the Rust engine's `with_model_ceiling` /
  `effective_max_tokens` so a policy/budget `max_tokens` can never exceed what the
  model can physically emit — otherwise a reasoning model burns its budget on
  reasoning and returns empty, or the upstream 400s (e.g. `groq-compound`'s 8192
  output cap under a 32768 budget). The ceiling is sourced from the gateway's
  `/model/info` by the consumer (kept out of the engine). EPIC th-1cc9fa.

## 0.20.3

### Patch Changes

- ab24904: feat(llm,python): clamp `max_tokens` to the model's output ceiling (Python parity)

  Python parity for the Rust `LlmClient` output-ceiling clamp. `AgentOptions` gains a
  `model_max_output: int | None` field and the engine now sends
  `effective_max_tokens(max_tokens, model_max_output)` =
  `min(max_tokens, ceiling)` on both the streaming and non-streaming chat paths (`None`
  / non-positive ceiling ⇒ graceful passthrough, no behaviour change). A new
  `effective_max_tokens` helper is exported for consumers. This stops a policy/budget
  `max_tokens` from exceeding what a model can physically emit — which otherwise makes
  a reasoning model burn its budget and return empty, or 400s upstream (e.g.
  `groq-compound`'s 8192 output cap). The ceiling is sourced from the gateway's
  `/model/info` by the consumer (the server), kept out of the engine. EPIC th-1cc9fa.

## 0.20.2

### Patch Changes

- 8c91101: feat(ts): clamp `max_tokens` to the model's output ceiling (TypeScript parity)

  The TypeScript core now mirrors the Rust engine's model-output ceiling clamp
  (EPIC th-1cc9fa). `AgentOptions` gains `modelMaxOutput?: number` and a new exported
  `effectiveMaxTokens(configured, ceiling?)` helper computes `min(maxTokens, ceiling)`
  (floored at 1, `undefined`/`0` ⇒ graceful passthrough). Every model call — both the
  non-streaming `run` and streaming `runStream` request builds — now sends the clamped
  value, so a budget/policy `maxTokens` (which may be tuned high) can never exceed what
  the model can physically emit. Without the clamp a reasoning model burns its budget
  on `reasoning_content` and returns empty `content`, or the upstream 400s (e.g.
  `groq-compound` caps output at 8192). The ceiling is sourced from the gateway's
  `/model/info` by the consumer (kept out of the engine so it takes no LiteLLM-specific
  HTTP). No behaviour change when `modelMaxOutput` is unset.

## 0.20.1

### Patch Changes

- d03fa10: feat(llm): clamp `max_tokens` to the model's output ceiling

  `LlmClient` gains `with_model_ceiling(Option<u32>)` + `effective_max_tokens()`.
  Every request now sends `min(config.max_tokens, model.max_output_tokens)` when a
  ceiling is known (`None` = graceful passthrough, no behaviour change). This lets a
  policy/budget `max_tokens` — which may be tuned high or resolved per-org via
  `@smooai/config` limits — never exceed what the model can physically emit, which
  otherwise makes a reasoning model burn its budget on `reasoning_content` and
  return empty, or 400s upstream (e.g. `groq-compound`'s 8192 output cap under a
  32768 budget). The ceiling is sourced from the gateway's `/model/info` by the
  consumer (kept out of the published engine so it takes no git-dep / no
  LiteLLM-specific HTTP). EPIC th-1cc9fa.

## 0.20.0

### Minor Changes

- c43816b: th-a62075: microVM isolation for SEP extensions (design + first increment).

  Closes the one structural gap the tool-layer guardrails cannot see: a
  compromised extension _binary_ issuing syscalls directly against the host
  kernel (process tampering, mount/bpf/kernel manipulation, credential-file
  reads) never crosses the SEP/tool channel, so the permission gate + Narc never
  observe it.

  **Approach: microsandbox microVM per extension** (`docs/Extension-Sandboxing-Design.md`).
  A seccomp/Landlock in-process tier was designed then dropped as
  over-engineering — microsandbox is stronger (separate guest kernel),
  cross-platform (macOS + Linux, unlike Linux-only seccomp), covers network
  egress natively, and is already on the fleet. It is driven by the `msb` CLI
  (runtime shell-out, like `smooth-dolt`), so operator-core gains **no cargo
  dependency**.

  This increment:

  - Manifest `[sandbox]` section (`image`, `memory`, `cpus`, `network` =
    `none`/`egress`, `allow_domains`) → `SandboxSpec` on `ExtensionManifest` and
    `SpawnSpec`.
  - Pure, exhaustively-tested `build_msb_command` argv builder (the isolation
    surface): `--no-net` by default, default-deny + per-domain `--net-rule allow@`
    for egress, empty-egress fails safe to no-net, scrubbed env forwarded as
    sorted `-e` pairs, image + attached-mode guest command.
  - `SMOOTH_EXTENSION_SANDBOX` gate (default **off** → direct host spawn,
    unchanged and non-breaking). When on + a `[sandbox]` image is present, the
    extension runs in a microVM; if `msb` is absent it **fails closed** rather
    than run untrusted code unisolated.
  - Extensions ship their code in the image (no writable host bind-mount — `msb`
    0.4.6 `-v` has no read-only mode); the image is the integrity anchor, so the
    host-binary `sha256` pin is skipped on the sandboxed path.

  Follow-ups: `--snapshot` pre-warming (th-4b4544), Events API (th-dd84b5),
  a smooth-provided base image.

## 0.19.1

### Patch Changes

- 9fcb1bc: th-64b1ee: audit + harden the `tool_search` meta-tool against prompt-injection tool promotion.

  Verified the critical defense claim: `PermissionHook` (a `ToolHook`) gates the
  _invocation_ of a promoted-but-forbidden tool. `ToolRegistry::execute` runs all
  pre-hooks before resolving the tool, and `tool_by_name` resolves promoted-deferred
  tools on the same path as eager ones — so a prompt-injection payload that makes a
  read-only agent `tool_search` a deferred `bash` exec tool cannot bypass the gate:
  the dangerous invocation is still denied. Added a regression test
  (`permission_hook_gates_promoted_deferred_tool`) that promotes a deferred `bash`
  via `tool_search`, then asserts a dangerous command is blocked (body never runs,
  execution counter stays 0) while safe calls still run.

  Also: `tool_search` now emits a `tracing::info!(target: "tool_search")` audit line
  for every promotion (query + promoted tool names) and returns the promoted names in
  its JSON payload (`promoted` field) so the privilege change is observable, not just
  a side-effecting log. Substring matching left as-is — the `MAX_MATCHES` cap plus the
  `PermissionHook` invocation gate are sufficient; no per-tool promote allowlist added.

## 0.19.0

### Minor Changes

- 4c1c39a: th-25ce5c: `AgentConfig::with_user_images` — stage image attachments for the current turn.

  A host that received a multimodal chat turn calls `.with_user_images(images)`; `run`/`run_with_channel` then attach them to that turn's user message (via `Message::user_with_images`). Empty by default, so text-only turns are unchanged. Completes the engine side of Big Smooth's vision support (epic th-3be564); the daemon consumes it to build image turns.

## 0.18.0

### Minor Changes

- 7d23573: th-22bfc1: Persist human approvals so the SEP permission gate stops being approve-once.

  Ports smooth's `wonk-allow.toml` allow-list into the engine (`permission_grants`
  module). The `PermissionHook` now consults the allow-list **before** prompting on
  an `Ask` verdict: a matching stored grant auto-approves silently, and answering
  `HumanResponse::ApprovedAlways` (a new additive variant) persists a grant so the
  next identical `Ask` never prompts again.

  - Two stacked TOML files, `~/.smooth/wonk-allow.toml` (user) and
    `<cwd>/.smooth/wonk-allow.toml` (project, wins on collision), format
    compatible in spirit with smooth's.
  - Grant kinds: `network` hosts (exact / `*.suffix` glob), `tools` (exact name),
    `bash` command prefixes (`"npm "`).
  - A grant can only upgrade an `Ask` — it can **never** waive a `Deny`
    circuit-breaker (`rm -rf /`, credential paths, dangerous domains, …).
  - Robust I/O: missing file → empty store, malformed file → surfaced error,
    atomic tempfile-then-rename writes.

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
