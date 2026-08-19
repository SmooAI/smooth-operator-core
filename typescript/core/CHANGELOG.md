# @smooai/smooth-operator-core

## 1.11.0

### Minor Changes

- 875b7a6: Ask the gateway for usage on every streaming request (`stream_options: {"include_usage": true}`), in all
  five engines.

  The OpenAI streaming API **omits usage unless it is explicitly requested**, and nothing here ever
  requested it. So the missing usage chunk was never the gateway losing data — it was the gateway
  correctly honouring a request that never asked. Everything built on top of that misattribution
  compensates for one unset request parameter: two char-count estimators, `prompt_tokens` hardcoded to
  `0`, `completion_tokens = content.len() / 4`, the `usage_estimated` and `cost_estimated` flags, and a
  cross-language "is this measured?" convention.

  Verified against llm.smoo.ai (LiteLLM 1.95.0, `groq-gpt-oss-120b`), same prompt both ways:

  - **without** the field — 7 chunks, **0** carrying usage
  - **with** the field — 8 chunks, **1** carrying `"prompt_tokens": 73, "completion_tokens": 8`

  Sent only when streaming: it is meaningless otherwise, and leaving it off keeps a non-streaming
  request byte-identical to before. Python honours an explicit caller-supplied `stream_options` rather
  than overriding it.

  Not fixed by this, and worth knowing: the per-request **cost** headers are present on a streamed
  response but all read `0.0`, because at header-flush time the completion has not been priced yet.
  `parse_gateway_cost` already maps `0` to `None`, so gateway cost stays unavailable on the streaming
  path and the `response_id` → `LiteLLM_SpendLogs.request_id` join remains the only authoritative
  per-turn cost there.

### Patch Changes

- 8c8fb7e: fix(python): three defects on the Python engine's streaming path

  All three lived only in `SmoothAgent.run_stream`. The non-streaming `run` was
  correct in every case, and Rust runs the same logic on both of its paths — which
  is precisely why these stayed invisible: `run_stream` is the path the polyglot
  servers, the TUI, and every real UI actually drive.

  - **The SEP `tool_call` hook was never folded in.** Streaming dispatched tools
    directly, so an extension that `Block`s `payments.refund` was honored
    non-streaming and IGNORED streaming, and `Modify` argument rewrites (redaction,
    scoping) were silently dropped. Both loops now fold through the one
    `_sep_tool_call_plan`, which takes either tool-call shape (the SDK object or the
    dict reassembled from stream deltas). The emitted `ToolCallEvent` carries the
    PLANNED arguments, so a rewrite that redacts a secret does not leak through the
    UI event either — matching what Rust emits.

  - **The model stream was never closed on early abandon.** A bare `async for` with
    no close meant a WS client disconnecting mid-answer unwound the generator on
    `GeneratorExit` and left the openai `AsyncStream`'s httpx response open until
    GC; under load the connection pool exhausted and later turns blocked on
    acquisition. The iteration is now wrapped so the stream is released on every
    exit, via `close()` (the openai SDK) or `aclose()` (a plain async generator).

  - **An abandoned turn checkpointed a torn conversation.** The `finally` runs on
    `GeneratorExit` too, so abandoning between a tool call and its result persisted
    an assistant message with `tool_calls` and no tool replies. Every provider
    rejects that ("an assistant message with tool_calls must be followed by tool
    messages"), and since each retry reloaded the same checkpoint the conversation
    was permanently wedged. Persistence (checkpoint and thread alike, on both
    loops) now drops a conversation left mid-tool-chain, so the store keeps its last
    good state and the next turn resumes from that — the invariant Rust gets from
    checkpointing only at well-formed points.

## 1.10.0

### Minor Changes

- 418d996: Carry cost/usage provenance and the gateway response id out of the engine, so nothing downstream can
  publish a fabricated number as a measurement.

  `collect_stream` invents a usage struct whenever a response carries no usage chunk — which LiteLLM
  does for every `smooth-*` alias — hardcoding `prompt_tokens = 0` and estimating `completion_tokens`
  as `content.len() / 4`. The estimate is kept, because budget enforcement needs something to multiply,
  but it is now flagged: `LlmResponse.usage_estimated`, aggregated onto `AgentEvent::Completed` as
  `usage_estimated`. This is what let production record `input_tokens = 0` on every streamed turn
  beside an output count that only ever tracked the reply's length. The old comment there claimed the
  estimate would "produce a real cost number against ModelPricing"; it does not, and it now says so.

  `AgentEvent::Completed` also gains `cost_estimated` — true once any call in the run was priced from
  the local `ModelPricing` table instead of the gateway's own figure. That table cannot price aliased
  routes and returns the free tier for anything it does not recognise, so a tainted total may be a wild
  under-count while looking exact. Cost and usage provenance are tracked separately on purpose: the
  gateway reports cost on an HTTP header and usage on an SSE chunk, so either can be authoritative
  while the other is a guess.

  Finally, the gateway's response id (`chatcmpl-…`, or `msg_…` on the Anthropic-native path) is now
  captured off both the streaming and non-streaming paths and surfaced as `LlmResponse.response_id` and
  `AgentEvent::Completed.response_id`. It was previously discarded at deserialization. It is the join
  key to `LiteLLM_SpendLogs.request_id`, whose row carries the gateway's authoritative dollars **and**
  its real prompt/completion counts — the only trustworthy source for either while the flags above can
  be set.

  All three fields cross the Temporal activity boundary via `ModelCallOutput`, so a durable replay
  cannot silently launder an estimate back into a measurement. `smooth-operator-temporal`'s core
  requirement moves 1.8 -> 1.9 accordingly.

  Note for consumers matching exhaustively on `StreamEvent`: this adds a `ResponseId` variant.

## 1.9.0

### Minor Changes

- 60c29b9: Add the .NET Temporal-backed durable execution backend (ADR-030), the C# sibling of the Rust
  `smooth-operator-temporal` crate. The new, optional `SmooAI.SmoothOperator.Temporal` package runs an
  agent turn as a Temporal workflow (`AgentTurnWorkflow`) whose model call and each tool invocation are
  Temporal activities, driving the engine's `AgentTurn.DriveTurnAsync` orchestration unchanged — one
  loop, two backends. It ships crash-safe resume, durable human-in-the-loop via `ApproveTool` /
  `DenyTool` signals, and a durable-wait timer, plus a `TemporalExecutor : IAgentExecutor` that swaps in
  behind the executor seam. The `Temporalio` SDK lives only in this separate package, so the published
  core stays zero-infra.

  This closes the last language gap in the parity docs' one honest exception — the durable-execution
  backend now ships for Rust **and** .NET, not Rust alone (the `AgentExecutor` seam was already in all
  five). Also removes `ConfigureAwait(false)` from `AgentTurn.DriveTurnAsync`, which its own contract
  promised is valid Temporal workflow code: inside the workflow scheduler that call posted the
  continuation off the single-threaded scheduler and hung the turn; in-process it has no captured
  context to marshal back to, so dropping it is behavior-neutral there.

### Patch Changes

- 9dbb8fd: Reconcile the parity docs now that the durable-execution backend ships in all five engines.

  The durable-execution **backend** (Temporal) is no longer a Rust-first exception. It now ships in all five languages, each as a separate, optional per-language package that mirrors the Rust `smooth-operator-temporal` crate: the Go `go/temporal` module (#170), TS `@smooai/smooth-operator-temporal` (#168), Python `smooai-smooth-operator-temporal` (#169), and .NET `SmooAI.SmoothOperator.Temporal` (#173). Each runs an agent turn as a Temporal `AgentTurnWorkflow` with the model call and tool invocations as activities, giving crash-safe resume, durable human-in-the-loop via approve/deny signals, and a durable-wait timer, and is verified by a skip-gated e2e against a real ephemeral Temporal server. The Temporal SDK stays isolated in the optional package, so no engine pulls it into your dependency tree.

  The README and `docs/Polyglot-Engines.md` now state the durable backend as in all five, and the only remaining Rust-first engine surface is the extension **sandbox / integrity hardening**. The honest ADR-030 follow-ups are kept and reframed as shared across all five (including Rust), not a Rust-vs-others gap: the durable path yields only a terminal result (no token-delta streaming) and reports `costUsd = 0` on the workflow result, and the executor seeds from agent config only — the workflow→streaming adapter bridge that closes these is still open.

## 1.8.12

### Patch Changes

- f4ca614: feat(go): optional Temporal-backed durable-execution backend for the Go engine (th-137b91, I parity)

  The durable-execution backend previously shipped only for Rust, while the `AgentExecutor` seam it
  plugs into lives in all five engines. This closes that gap for Go: a new **separate Go module**,
  `github.com/SmooAI/smooth-operator-core/go/temporal`, runs an agent turn as a Temporal workflow whose
  side-effects — the model call and each tool invocation — are Temporal activities. The workflow drives
  the engine's deterministic `core.DriveTurn` orchestration **unchanged** (the activities delegate to
  `core.InProcessActivities`, the same side-effect code the in-process path runs inline), so the durable
  path and the in-process path are the same loop — no second agent loop to keep in sync.

  It mirrors the Rust `smooth-operator-temporal` crate: `AgentTurnWorkflow` + `AgentTurnActivities`,
  durable human-in-the-loop (an `ApprovalRequiredTools` tool blocks on `workflow.Await` until an
  `approve_tool` / `deny_tool` signal names its call id; a denial returns a tool-error result without
  executing the tool), and a durable-timer `WaitTool` (a `wait` call sleeps the workflow on a Temporal
  timer that can span days). Being a **separate module** is the mirror of the Rust crate's `temporal`
  cargo feature — the engine's default build pulls in none of the Temporal SDK and stays zero-infra;
  only a consumer that imports this module takes on `go.temporal.io/sdk`.

  Tests: a serde-boundary unit test that runs without Temporal, plus four e2e tests (health, real agent
  turn, durable timer, HITL approve/deny) that run against a real ephemeral dev server and self-skip when
  none is reachable — the Go mirror of the Rust `*_e2e.rs`. The core CI Go job now covers the new module.

## 1.8.11

### Patch Changes

- 2561905: Reconcile the parity documentation with what is actually merged.

  Each parity workstream updated the feature list as it landed, which left the list accurate line-by-line but wrong in aggregate: multimodal images were still listed as "still being ported" after they shipped in all five, and seven capabilities that are in all five engines today (multimodal input, the tool-hook lifecycle, the permission gate + deny-policy + grants, the SEP extension host, and gateway cost headers) were missing from the list entirely.

  Every claim in the revised list was re-verified against merged code by symbol, not by reading the PR that added it. The two remaining honest exceptions are now stated up front in the README rather than buried: the extension **sandbox / integrity hardening** is Rust-first (capability declarations are honoured in all five; process-level confinement and manifest-integrity verification are not), and the durable-execution **backend** ships only for Rust while the `AgentExecutor` seam it plugs into is in all five.

  Also documents the three shared corpora — eval scenarios, the Narc detection set, and the provider-routing table — as the mechanism behind the parity claim: each is generated from the Rust reference and replayed by all five engines including Rust, so the reference cannot drift away from its own ports unnoticed.

- fa2aba7: Add a Temporal-backed durable execution backend for TypeScript — the sibling of the Rust `smooth-operator-temporal` crate (ADR-030, parity item I).

  A new **optional** package, `@smooai/smooth-operator-temporal`, runs an agent turn as a Temporal **workflow** whose model call and each tool invocation are Temporal **activities**. The workflow drives the engine's deterministic `driveTurn` orchestration **unchanged**, so the durable path and the in-process path are the _same loop_ — the durable path just gets crash-safe resume, durable human-in-the-loop via `approveTool` / `denyTool` signals, and durable timers (an agent that pauses itself on a Temporal timer and resumes). `TemporalAgentExecutor` implements the engine's `AgentExecutor` interface, a drop-in for `InProcessExecutor`.

  Kept off the default path exactly like the Rust crate's off-by-default `temporal` cargo feature: the published `@smooai/smooth-operator-core` pulls in no Temporal SDK. The activity DTO boundary carries no Temporal dependency and is unit-tested without a runtime; the full e2e (health, agent turn, durable timer, HITL) runs against an ephemeral Temporal test server and self-skips offline.

  Core adds a small `./executor` subpath export so the workflow bundle can import the pure `driveTurn` loop (a type-only, zero-runtime-dependency entry point) without pulling the whole engine into Temporal's deterministic workflow sandbox.

## 1.8.10

### Patch Changes

- a893804: feat(rust): publish `smooai-smooth-operator-temporal` to crates.io

  The Temporal durable-execution backend (ADR-030) was `publish = false`, which meant
  nothing outside this repo could depend on it. That was the sole blocker on
  `smooth-operator-server` selecting a durable backend: the server consumes the engine
  from crates.io and is itself published, and cargo refuses to publish a crate that
  declares a git or path dependency — even an optional one behind an off-by-default
  feature, because packaging validates the manifest rather than the resolved feature
  set. Brent's call was to publish rather than work around it, accepting that the
  pinned preview SDK becomes publicly visible and publicly versioned.

  What changed:

  - `rust/smooth-operator-temporal/Cargo.toml` — dropped `publish = false`, added the
    metadata crates.io wants (`repository`, `readme`, `keywords`, `categories`), and
    gave the core dependency a version requirement alongside its path. That
    requirement is `^1.8` rather than the exact lockstep version so it stays
    satisfiable across patch releases without needing to be re-synced.
  - Brought the crate into the version lockstep. It was drifting at `0.14.0` while
    every other artifact was on the `1.8.x` anchor, which was harmless while its
    version was inert but would now make `ci-publish`'s `cratesHasVersion` check never
    match — it would try to publish the stale version forever. `sync-versions.mjs`
    gained an anchor for it, so it moves with the anchor like the other five manifests.
    Its first published version is therefore `1.8.x`; since it has never been
    published, no public version history is disturbed.
  - `ci-publish.mjs` publishes it, ordered **after** the core crate — cargo will not
    publish a crate whose dependency is not yet on the index.
  - `pr-checks.yml` gained its own `cargo publish --dry-run` for it. Packaging rules
    are per-manifest, so the core crate's existing dry-run cannot see a bad manifest
    here; without this, a stray path dep or missing metadata would surface only at
    release and take the whole lockstep publish down with it. That is exactly how the
    original blocker stayed hidden.

  A default build still compiles no `temporalio-*` at all — the SDK stays behind the
  off-by-default `temporal` feature, so consumers who do not opt in remain zero-infra.
  Verified: the crate packages and its verify build compiles against the **published**
  core from crates.io (not the local path), and under `--features temporal` clippy is
  clean and all four e2e pass against a real ephemeral Temporal server (1.31.2),
  including the durable HITL signal gate and the durable timer.

## 1.8.9

### Patch Changes

- fc19391: feat(ts,python,dotnet): send Anthropic `cache_control` markers on the request

  Completes prompt-caching parity. The previous change ported the `PromptCache`
  static/dynamic split to all five engines but landed the wire half — the
  `cache_control` markers that make Anthropic actually cache the prefix — in Rust
  and Go only, because TypeScript, Python and .NET had no request builder of their
  own to attach them to. Their gateway clients have since shipped, so all five now
  send the markers.

  Marker placement matches Rust exactly: `ephemeral` on the last system message
  (rewritten into Anthropic block form), on the **last** tool in the tools array
  (the highest-ROI breakpoint — the tool registry is large and near-constant within
  a run), and on the last history message so turn-by-turn caching extends. Marking
  a block caches that block plus everything before it, so only the last block of
  each reused prefix carries one.

  The gate is the same everywhere: a Claude-ish model id _or_ a known Claude-routing
  `smooth-*` alias, AND a LiteLLM-style gateway or `anthropic.*` base URL. Bare
  OpenAI/Gemini/Groq endpoints 400 on unknown extension fields, and `smooth-fast`
  routes to Groq, so both stay off.

  **Where the base URL comes from.** The gate needs the route, which the engine
  previously had no way to see — the agent is handed a client, not a URL. Each
  client now reports its own: TypeScript's `ChatClientLike` gained an optional
  `apiBaseUrl` that `gatewayClientFrom` fills from the OpenAI SDK, Python's
  `GatewayLlmProvider` exposes `api_base_url`, and .NET reads its existing
  `_endpoint`. A client that doesn't report one — every mock — never gets markers,
  so existing tests and any hand-rolled client are unaffected.

  **Multimodal content is passed through, not flattened.** Rust learned this the
  hard way (pearl th-25ce5c): wrapping a message that carries image parts into a
  text block silently drops the images, and the last message in a vision turn is
  exactly the image-bearing one. All three ports detect a non-text part and leave
  the content untouched; caching only applies to text prefixes anyway. Empty
  content (a tool-call-only assistant message) is likewise left alone, and content
  already in block form has its stale marker cleared before the last block is
  re-marked.

  The marking logic lives in a standalone module per language (`cacheControl.ts`,
  `cache_control.py`, `CacheControl.cs`) so the agent/request path needed only a
  single gated call — deliberate, to keep the diff in those heavily-shared files to
  a couple of lines.

  29 new tests (8 TypeScript, 8 Python, 13 .NET), including end-to-end assertions
  that an agent driven by a client reporting no base URL sends a body with no
  `cache_control` anywhere.

## 1.8.8

### Patch Changes

- 6f5144c: feat(ts,python,dotnet): multimodal image attachments on user messages

  Completes the multimodal port (pearl th-25ce5c) — Rust had it, Go landed in #147,
  and these are the remaining three. A host that receives a chat turn carrying images
  sets `nextUserImages` / `next_user_images` / `NextUserImages`; the agent attaches
  them to that one turn's user message, and the body-assembly site emits OpenAI
  `image_url` content parts.

  Wire shape matches Rust exactly in all four: text part first (omitted when the text
  is empty, since images may be sent alone), then one `image_url` part per image, in
  order, with `detail` omitted when unset.

  **A turn without images is byte-identical to before** — `content` stays a plain
  string unless a user message actually carries images. Each language's tests lead
  with that negative case.

  Logic lives in a standalone module per language (`multimodal.ts`, `multimodal.py`,
  `Multimodal.cs`) with a single call at the shared body-assembly line, following the
  convention prompt caching established — so the three workstreams on that line rebase
  past each other instead of colliding.

  .NET differs in shape, because its assembly site is `GatewayChatClient.BuildMessages`
  rather than the agent: images ride agent→client through MEAI's content model as an
  `ImageUrlContent : AIContent`. MEAI's own `DataContent`/`UriContent` nearly fit but
  cannot express the OpenAI `detail` hint the other engines support. `BuildMessages`
  also had to stop treating an empty-text message as skippable — a turn may carry
  images alone, and the old guard would have dropped it silently.

  **Interaction with prompt caching, which is the real hazard here.** cache_control
  marks the LAST message in history, which in a vision turn IS the image-bearing one;
  flattening it into a text block would silently drop the images. The caching ports
  already guard this by passing content through untouched when any part has a `type`
  other than `"text"` — so these parts always emit `type`, and each language now has a
  regression test driving a vision turn through a Claude-routing gateway and asserting
  the image survives. Verified by mutation: removing the discriminator makes that test
  fail, which is exactly the silent failure it exists to catch.

## 1.8.7

### Patch Changes

- bd413b3: Port provider routing to the Go, TypeScript, Python and .NET engines.

  Routing was Rust-only, so the other four engines could talk to a gateway but had no say in _which_ model a given call should use — every activity got whatever single client the consumer wired up. Each engine now ships the same three pieces:

  - **`ProviderRegistry`** — provider credentials/URLs plus a per-activity routing table. Six semantic slots (`Coding`, `Reasoning`, `Reviewing`, `Judge`, `Summarize`, `Fast`) each resolve to a `ModelSlot`, and resolution walks the slot's fallback chain until it finds a registered provider. An unregistered provider with no fallback is an error, never a silent substitution to somewhere else. Five presets (Smoo AI gateway, OpenRouter/LLM Gateway low-cost, OpenAI, Anthropic) and nine provider factories are included; the hosted gateway is opt-in and never the default.
  - **Per-model wire quirks** — case-insensitive substring lookup on the concrete upstream name, so minor version drift still hits its entry.
  - **LiteLLM alias resolution** — `GET /model/info` recovers the gateway's `alias → upstream` map, alias-sorted so diagnostics print the same order every run.

  The registry reads and writes the same `~/.smooth/providers.json` the Rust CLI does: snake_case keys, optional slots omitted rather than written as null, and the legacy `thinking` / `planning` field names still migrating onto the merged `reasoning` slot. Each engine also gains a `clientFor(activity)` bridge that turns a resolved route into that language's gateway client, refusing an Anthropic-dialect provider rather than speaking OpenAI's wire format at it.

  Routing is the expensive place to diverge — a slot resolving to the wrong model or base URL sends real traffic and real money somewhere nobody intended, and it looks like it is working. So the values are pinned by a shared corpus at `spec/providers/routing.json`, generated from the Rust reference: all five engines (Rust included) replay it and assert every preset slot resolves to the same model, URL, key and wire format, that quirks match, and that URL-building and alias parsing agree.

## 1.8.6

### Patch Changes

- beedd6d: fix(rust): clear the clippy 1.96 `unnecessary_sort_by` failures in `checkpoint.rs`

  Both checkpoint stores sorted newest-first with `sort_by(|a, b| b.created_at.cmp(&a.created_at))`,
  which clippy 1.96 rejects in favour of `sort_by_key(|c| Reverse(c.created_at))`.
  Same ordering, no behavior change — but it is a hard error under `-D warnings`,
  so main goes red the moment GitHub's stable runner reaches 1.96. It is invisible
  today only because the clippy step is `continue-on-error`.

  `cargo clippy --workspace --all-targets -- -D warnings` and the `temporal`-feature
  clippy both exit 0 on 1.96 now; 643 tests still pass.

## 1.8.5

### Patch Changes

- 90a3491: feat(go): multimodal image attachments on user messages

  Ports the Rust reference's multimodal turns (pearl th-25ce5c) to the Go engine.
  A host that receives a chat turn carrying images sets `AgentOptions.NextUserImages`;
  the agent attaches them to that one turn's user message, and the client emits them
  as OpenAI `image_url` content parts — the standard shape every model we route
  vision to (gemini-flash, gpt-4o, mimo-vl) speaks.

  - `ImageContent{URL, Detail}` — a `data:` URL (`data:image/png;base64,...`) or a
    remote `https` URL, plus the optional OpenAI vision hint, omitted when empty.
  - `ChatMessage.Images` — attachments on a USER message.
  - `AgentOptions.NextUserImages` — the Go sibling of `AgentConfig::with_user_images`.

  Wire shape matches Rust exactly: text part first (omitted when the text is empty,
  since images may be sent alone), then one `image_url` part per image, in order.

  **A turn without images is byte-identical to before.** `content` stays a plain
  string unless a user message actually carries images — the negative case is what
  the tests lead with, since every existing text-only turn depends on it.

  Also pays off the `ponytail:` note prompt caching left behind: `wrapWithCacheControl`
  now passes a content-parts array through untouched. Flattening it into a text block
  would have silently dropped the images, and caching only applies to text prefixes
  anyway (same reasoning as Rust's `wrap_with_cache_control`).

  TypeScript, Python and .NET are deliberately NOT included — their request layers
  are being rewritten by the http-client-parity work, and porting into a moving
  target would just create conflicts. They follow once that lands.

## 1.8.4

### Patch Changes

- 92f9590: feat(ts,python,dotnet): send Anthropic `cache_control` markers on the request

  Completes prompt-caching parity. The previous change ported the `PromptCache`
  static/dynamic split to all five engines but landed the wire half — the
  `cache_control` markers that make Anthropic actually cache the prefix — in Rust
  and Go only, because TypeScript, Python and .NET had no request builder of their
  own to attach them to. Their gateway clients have since shipped, so all five now
  send the markers.

  Marker placement matches Rust exactly: `ephemeral` on the last system message
  (rewritten into Anthropic block form), on the **last** tool in the tools array
  (the highest-ROI breakpoint — the tool registry is large and near-constant within
  a run), and on the last history message so turn-by-turn caching extends. Marking
  a block caches that block plus everything before it, so only the last block of
  each reused prefix carries one.

  The gate is the same everywhere: a Claude-ish model id _or_ a known Claude-routing
  `smooth-*` alias, AND a LiteLLM-style gateway or `anthropic.*` base URL. Bare
  OpenAI/Gemini/Groq endpoints 400 on unknown extension fields, and `smooth-fast`
  routes to Groq, so both stay off.

  **Where the base URL comes from.** The gate needs the route, which the engine
  previously had no way to see — the agent is handed a client, not a URL. Each
  client now reports its own: TypeScript's `ChatClientLike` gained an optional
  `apiBaseUrl` that `gatewayClientFrom` fills from the OpenAI SDK, Python's
  `GatewayLlmProvider` exposes `api_base_url`, and .NET reads its existing
  `_endpoint`. A client that doesn't report one — every mock — never gets markers,
  so existing tests and any hand-rolled client are unaffected.

  **Multimodal content is passed through, not flattened.** Rust learned this the
  hard way (pearl th-25ce5c): wrapping a message that carries image parts into a
  text block silently drops the images, and the last message in a vision turn is
  exactly the image-bearing one. All three ports detect a non-text part and leave
  the content untouched; caching only applies to text prefixes anyway. Empty
  content (a tool-call-only assistant message) is likewise left alone, and content
  already in block form has its stale marker cleared before the last block is
  re-marked.

  The marking logic lives in a standalone module per language (`cacheControl.ts`,
  `cache_control.py`, `CacheControl.cs`) so the agent/request path needed only a
  single gated call — deliberate, to keep the diff in those heavily-shared files to
  a couple of lines.

  29 new tests (8 TypeScript, 8 Python, 13 .NET), including end-to-end assertions
  that an agent driven by a client reporting no base URL sends a body with no
  `cache_control` anywhere.

## 1.8.3

### Patch Changes

- 0db4b16: feat(go,ts,python,dotnet): port the durable-execution seam (ADR-030) to all four ports

  The Rust reference has carried the ADR-030 seam for a while — `AgentExecutor` (where
  and how a turn runs) over `AgentActivities` + `drive_turn` (the side-effecting model
  and tool calls, split from the deterministic loop that sequences them). The four ports
  had neither, so a durable backend had nowhere to plug in outside Rust.

  Each port now ships both halves, in its own idiom:

  - **Go** — `AgentExecutor` / `InProcessExecutor` / `AgentActivities` / `DriveTurn` /
    `InProcessActivities` in `go/core/executor.go`.
  - **TypeScript** — the same names from `@smooai/smooth-operator-core` (new
    `src/executor.ts`, re-exported from the barrel).
  - **Python** — `AgentExecutor` / `InProcessExecutor` / `AgentActivities` /
    `drive_turn` / `InProcessActivities` from `smooth_operator_core`.
  - **.NET** — `IAgentExecutor` / `InProcessExecutor` / `IAgentActivities` /
    `AgentTurn.DriveTurnAsync` / `InProcessActivities`.

  Nothing changes for existing code. Every in-process executor is a verbatim delegation
  to the agent's existing run entry point, so a consumer that never mentions an executor
  gets byte-for-byte the turn it got before, and each port carries the Rust
  in-process-identical parity test to keep it that way. `DriveTurn` reproduces the Rust
  loop statement for statement — model call, assistant-message push condition, tool
  results paired to their calls, iteration bound treated as a stop rather than an error —
  so a durable backend and the inline path stay the same loop.

  Two deliberate divergences from Rust, both documented at their definition: the default
  iteration bound tracks each port's own agent default (8 in TS/Python/.NET, 20 in Go)
  rather than Rust's 50, so introducing the seam can't change how long a turn may run;
  and the assistant-push condition drops Rust's `reasoning_content` arm in the ports
  whose model response carries no such field.

  No new dependencies in any port. A Temporal-backed backend stays out of the engine
  package by design — each port carries a `TODO(ADR-030)` naming the separate opt-in
  package it belongs in, mirroring the Rust `smooth-operator-temporal` crate behind its
  off-by-default `temporal` cargo feature.

- 020250f: feat(ts,python,dotnet): structured output — `response_format` + JSON parsing, completing the five-engine set

  Finishes the port Go started (core#130). Structured output — a guaranteed-JSON
  answer conforming to a caller-supplied JSON Schema — now exists in all five
  engines, so `docs/Polyglot-Engines.md` stops listing it among the Rust-only
  surfaces and gets a row of its own.

  Each engine uses its own idiom rather than a transliterated Rust enum:

  - **TypeScript** — `jsonSchemaFormat(name, schema)` + `responseFormatField(format)`,
    a spreadable fragment matching this package's existing `metadataField`, so an
    unset format contributes nothing: `JSON.stringify({model, ...responseFormatField()})`
    is `{"model":"m"}`. `structuredJson<T>(response)` parses it back.
  - **Python** — `json_schema_format` + `response_format_field` returning a mapping
    to splat into `chat.completions.create`, mirroring the `**({"metadata": ...})`
    idiom already there. `structured_json(response)` parses it back.
  - **.NET** — **no new type at all.** `Microsoft.Extensions.AI` already models this
    as `ChatOptions.ResponseFormat` / `ChatResponseFormat.ForJsonSchema`, so
    `GatewayChatClient` maps the platform's own `ChatResponseFormatJson` onto the
    wire object; `StructuredJson()` / `DeserializeJson<T>()` are extension methods on
    `ChatResponse`. Inventing a parallel `ResponseFormat` beside the platform's would
    have been a second way to say the same thing.

  **Byte-identical when unset** in all three, each with a test that fails if the
  field ever starts serializing as `null` or `{}`.

  Two deliberate deviations, both because matching Rust's _signature_ would have
  meant not matching its _behavior_:

  - **No `deserializeJson` in TypeScript or Python.** Rust's version validates
    against a concrete type through `serde`. TypeScript has no runtime types and
    Python has no equivalent without a validation dependency, so there it would be
    `structuredJson` plus a cast — an API implying a guarantee the engine cannot
    make. Go and .NET, which really can deserialize, have it.
  - **.NET sends `{"type":"json_object"}` for a schemaless `ChatResponseFormat.Json`.**
    Rust has no such variant, but the platform type makes it reachable, and silently
    dropping a caller's explicit request for JSON mode is worse than sending the
    field OpenAI defines for it.

  Rust's Anthropic-native path (no `response_format` there, so it forces a synthetic
  tool whose `input_schema` IS the schema) has no analogue in the four ports, which
  talk to OpenAI-compatible endpoints only. Stated in each port's source rather than
  silently omitted.

  28 new tests across the three: TypeScript 279 passed, Python 317 passed, .NET 285
  passed.

- 103f4c6: docs(rust,go,ts,python,dotnet): state that sub-workflows are first-class graph vertices

  Follow-up to the sub-workflow primitive. The contract already held — each engine's
  `sub_workflow_node` returns that engine's own node type, so `add_edge` /
  `add_conditional_edge` cannot tell a sub-workflow apart from a plain node — but
  nothing said so, and no test proved it.

  Now documented and covered: plain nodes and sub-workflows are **interchangeable
  vertices of one composite graph**. A sub-workflow works as an edge source and as
  an edge target alike, including as the target of a conditional edge and as the
  node a conditional router leaves (terminate sentinel included), and the nesting is
  arbitrary — a sub-workflow may itself contain sub-workflows.

  Each engine gains the same composite-graph test: a parent wiring
  `plain --conditional--> sub-workflow --conditional--> plain`, where that
  sub-workflow's own vertices are a plain node plus a further nested sub-workflow
  (depth 2), all running inside one parent step.

  No behavior change — tests and doc comments only.

## 1.8.2

### Patch Changes

- 8647a47: fix(dotnet): ship a default pricing table so an unpriced model isn't silently $0

  C# was the only engine with **no local pricing fallback at all**. `AgentOptions.Pricing`
  starts empty and `SmoothAgent.LookupPricing` returned null for anything not in it,
  so `CostTracker.Record` charged exactly $0 on every call unless the caller had
  hand-registered the model. A consumer could not tell "this model is free" from
  "nobody told me the price".

  Go, Python and TypeScript have shipped a `DefaultPricing` / `DEFAULT_PRICING`
  table for this since the cost work landed, and their `record(...)` falls back to
  it when the caller passes none. C# now carries the same two entries at the same
  prices, behind `ModelPricing.Default`, plus a `ModelPricing.ForModel(modelId,
overrides)` resolver that mirrors the siblings' "caller table, then default
  table, then unpriced" lookup.

  Precedence is unchanged and unsurprising: the gateway's authoritative
  per-request cost beats everything, then an `AgentOptions.Pricing` entry, then
  `ModelPricing.Default`, then unpriced.

  Deliberately **not** in scope: unifying the five engines' local tables. They
  genuinely disagree about coverage — Rust's substring resolver prices `gpt-4o`,
  `deepseek`, `gemini-flash` and the `smooth-*` aliases but **not** Claude, while
  these three price **only** Claude. Closing that needs a cited price list rather
  than an inference, since a wrong estimate mis-bills silently.

## 1.8.1

### Patch Changes

- dc17e0d: feat(go,ts,python,dotnet,rust): port prompt caching — the static/dynamic split everywhere, `cache_control` wire markers in Go

  Prompt caching in the Rust reference is two separable pieces, and they had
  different parity gaps:

  **1. The static/dynamic system-prompt split (`PromptCache`) — now in all five.**
  A system prompt has two halves with very different churn rates: role
  instructions and tool schemas barely change, while project context
  (AGENTS.md / CLAUDE.md) changes every turn. Anthropic's cache keys on a
  _prefix_, so putting the volatile half first invalidates everything.
  `__PROMPT_CACHE_BOUNDARY__` splits them — above it is static and hashed once for
  cache-key dedup, below it is dynamic and swappable via `updateDynamic` without
  busting the static prefix. `fullPrompt()` reassembles it for the agent's
  instructions, and a prompt with no marker round-trips unchanged (all dynamic,
  nothing falsely claimed cacheable).

  This was `pub` inside Rust's `conversation` module but **never re-exported at the
  crate root**, so no embedder could reach it. Rust now exports `PromptCache`
  alongside `Conversation`.

  **2. The `cache_control` request markers — Rust + Go.** `ephemeral` markers go on
  the three strategic prefix boundaries: the last system message (rewritten into
  Anthropic block form), the LAST tool in the tools array (the highest-ROI
  breakpoint — the tool registry is large and near-constant within a run), and the
  last history message (so turn-by-turn caching extends). Gated exactly like Rust:
  a Claude-ish model id _or_ a known Claude-routing `smooth-*` alias, AND a
  LiteLLM-style gateway or `anthropic.*` base URL. Bare OpenAI/Gemini/Groq
  endpoints 400 on unknown extension fields, and `smooth-fast` routes to Groq, so
  both stay off.

  **TypeScript, Python and .NET do not get the wire half yet** — deliberately, not
  by oversight. Those three take an injected chat client and have no request
  builder of their own, so there is literally nowhere to attach a marker until the
  native HTTP client lands. They get the full `PromptCache` split now and the
  markers when that request layer merges.

  **Byte-identical when off.** Go's `wireMessage.Content` widened from `string` to
  `any` to carry either a plain string or a block array; a string in an `any` field
  marshals identically, `cache_control` fields are `omitempty`, and the gate needs
  an api base it is only given by the real client. A regression test asserts that
  a request built with no URL is byte-for-byte the same as one to a gated-off
  upstream, and that neither contains `cache_control`.

  One deliberate divergence: `staticHash()` is FNV-1a in the ports rather than
  Rust's `DefaultHasher`, which is not reproducible across languages or even
  across Rust releases. The hash is process-local cache-key dedup and never goes
  on the wire, so the ported contract is the behavior — same static text hashes
  the same, different text differs, `updateDynamic` never changes it — not the
  literal value.

  Rust's tests ported: 7 `PromptCache` tests to each of the four ports, plus Rust's
  `cache_control` gate and request-body tests to Go (37 new tests total).

## 1.8.0

### Minor Changes

- 6a73d29: feat(rust,go,ts,python,dotnet): sub-workflows that run to completion inside one turn

  The typed `Workflow` graph has always had two speeds. Standalone, `run()` executes
  the whole graph. But when a workflow drives a **conversation**, the driver advances
  it turn-by-turn — one node per user turn — so a five-node graph costs five turns
  even when four of them are pure computation the user never needs to see.

  This adds the composition primitive for the other half: a **sub-workflow node**
  that wraps a child `Workflow` and runs it to completion — every node, its
  conditional edges, and the `__end__` sentinel — inside ONE parent step. The
  top-level graph stays turn-gated exactly as before; sub-workflows are purely
  additive, and nothing about the existing turn-by-turn behavior changes.

  Typed state composes across the boundary: `map_in` projects parent state into the
  child's state type (they need not be the same type), `map_out` folds the child's
  final state back. An error from any child node propagates out of the parent's
  `run` — a failed sub-graph fails the turn, it does not silently return a partial
  state. Sub-workflows nest, because a sub-workflow node is just a node: a child may
  itself hold one.

  Same semantics, same test set, in all five engines:

  - **Rust** — `sub_workflow_node(name, child, map_in, map_out) -> FnNode<P>`
  - **Go** — `SubWorkflowNode[P, C](child, mapIn, mapOut) NodeFn[P]`
  - **TypeScript** — `subWorkflowNode<P, C>(child, mapIn, mapOut): NodeFn<P>`
  - **Python** — `sub_workflow_node(child, map_in, map_out)`
  - **.NET** — `Workflow.SubWorkflowNode<TParent, TChild>(child, mapIn, mapOut)`

  It reuses each engine's existing node seam rather than adding a parallel runner, so
  a sub-workflow is indistinguishable from any other node to the graph around it —
  which is what makes the nesting fall out for free.

## 1.7.16

### Patch Changes

- 73e2b2b: fix(dotnet): record usage and cost on `RunStreamingAsync` — and make `Budget` actually stop a streamed turn

  `SmoothAgent.RunStreamingAsync` declared no `UsageDetails` and no `CostTracker`.
  It folded each iteration's updates with `updates.ToChatResponse()` — which
  materializes both `Usage` and `ModelId` — and then threw them away, so a streamed
  turn reported no tokens, no cost, and, worst of all, **`AgentOptions.Budget` was
  silently inert**: a runaway streaming turn could not be stopped by its own spend
  ceiling. `RunAsync` had done all three since the cost work landed; only the
  streaming path was missing them.

  The streaming loop now mirrors `RunAsync` exactly: `Accumulate` the usage,
  `RecordWithGatewayCost` (so the gateway's authoritative cost still wins over the
  local pricing table), and break on `ExceedsBudget`.

  **New public API — `SmoothAgent.LastRunResponse`.** `RunStreamingAsync` returns
  `IAsyncEnumerable<ChatResponseUpdate>`, which has nowhere to hang a turn total,
  so this property is C#'s stand-in for the terminal event the sibling engines emit
  on the stream itself (Rust `AgentEvent::Completed`, Go/Python/TypeScript `done`,
  all carrying `cost_usd` and the token totals). It is null until the stream is
  fully enumerated, is reset when a new streaming turn begins — so a stale total
  can never be mistaken for a fresh one — and `RunAsync` does not touch it.

  Downstream: `SmooAI/smooth-operator`'s C# server sums `UsageContent` chunks by
  hand and then hardcodes `costUsd: 0` on `eventual_response.usage`. It can now
  read a real number from the engine instead.

## 1.7.15

### Patch Changes

- 9fc7a1e: fix(rust,go,ts,python,dotnet): make a scripted mock response report the SAME usage in all five engines

  The five `MockLlmProvider`s disagreed about what a scripted turn reports, so the
  shared server scenario corpus in `SmooAI/smooth-operator` could not assert
  `eventual_response.usage` at all — it documented five different answers instead
  of one invariant:

  | engine                   | promptTokens | completionTokens |
  | ------------------------ | ------------ | ---------------- |
  | Go · Python · TypeScript | 0            | 0                |
  | Rust                     | 0            | 5                |
  | C#                       | 10           | 5                |

  Rust's `0/5` was not even a mock decision: the mock reported `0/0`, and the
  streaming path's "no `StreamEvent::Usage` arrived" estimator (~4 chars/token)
  then invented 5 completion tokens from the scripted reply's length — so the
  number moved whenever a scenario's text changed.

  All five now report **10 prompt / 5 completion / 15 total** (C#'s existing
  convention, the only one that was deliberate), exposed as a named helper so the
  number has one definition per engine and the corpus can cite it:
  `llm_provider::scripted_usage()` (Rust), `ScriptedUsage()` (Go),
  `scripted_usage()` (Python), `SCRIPTED_USAGE` (TypeScript),
  `MockLlmProvider.ScriptedUsage()` (C#).

  **Only the FIFO scripting helpers attach it** — `push_text`/`push_tool_call` and
  their per-language spellings. The benign empty reply a _drained_ script falls
  back to still reports nothing, so "the script ran out" stays distinguishable
  from "the model answered". An explicit usage argument still overrides
  (Go's `WithUsage`, Python's `usage=`, TypeScript's `usage`).

  Each engine gains a unit test pinning the convention, so a drift goes red here
  rather than as a confusing cross-language corpus failure downstream.

- cd43793: Port the NarcHook secret-detection + prompt-injection scanner to the Go, TypeScript, Python and .NET engines.

  The scanner was Rust-only, which meant the extension boundary in the other four engines passed tool-call arguments to a subprocess unscanned and handed the subprocess's result back to the model verbatim — no check for leaked credentials, no check for injection payloads. Each engine now installs the same `ToolHook`:

  - **`pre_call`** scans the arguments. A Block-severity injection match (the active data/URL exfiltration signals) blocks the call before the tool runs. Lower-severity injection and any secret are alerted, not blocked — a tool argument legitimately carrying a secret is common enough that a hard block there would be a footgun.
  - **`post_call`** scans the result and **redacts**: a secret in a tool result raises a Block alert and is rewritten to `[REDACTED:<pattern-name>]` in place, so the model never sees the raw credential. Injection in a result stays surveillance-only and is never rewritten.

  Detection is 10 credential patterns (AWS keys, private keys, bearer tokens, provider keys, Stripe keys, …) and 8 injection patterns (instruction override, role hijack, system-prompt spoofing, jailbreak, base64 smuggling, data/URL exfiltration, suspicious hosts).

  Because a security hook that is weaker in one language is a real gap, the detection set is now pinned by a shared corpus at `spec/narc/corpus.json` — 39 vectors of positive/near-miss pairs, generated from the Rust reference. All five engines (Rust included) replay it and assert identical findings, in identical order, at identical severities, so dropping a pattern or downgrading a severity fails that language's test suite instead of silently shipping.

## 1.7.14

### Patch Changes

- 86e7a3a: feat(go): structured output — `response_format` on the request, JSON helpers on the response

  First port of the Rust reference's structured output (SMOODEV-1472) — a
  guaranteed-JSON answer conforming to a caller-supplied JSON Schema. None of the
  four ports had it; Go goes first because its `GatewayClient` owns its own HTTP
  request builder and is stable on main. TypeScript, Python and .NET follow once
  the in-flight HTTP/request-layer work lands there, so this adds a field to a
  settled builder instead of racing one.

  - **`ResponseFormat`** (`go/core/structured.go`) + `JSONSchemaFormat(name, schema)`,
    the analogue of Rust's `ResponseFormat::json_schema` (strict by default). Rust
    models this as a single-variant enum; Go uses a struct until a second variant
    exists.
  - **`ChatRequest.ResponseFormat`** serializes as
    `response_format: {"type":"json_schema","json_schema":{name,schema,strict}}`.
    Pointer + `omitempty`, so an unset format leaves the request **byte-identical**
    to before — that is the parity bar, and there is a test that fails if the field
    ever starts emitting `null`.
  - **`ChatResponse.StructuredJSON()` / `.DeserializeJSON(&target)`** mirror Rust's
    `structured_json` / `deserialize_json`, including the error text and the
    200-character (not byte) content snippet, so a model that ignores the schema is
    diagnosable from the error alone rather than surfacing as an empty value.
  - The mock records the requested format for free — Go's `MockLlmProvider`
    captures the whole `ChatRequest` — which is what Rust's `RecordedCall.response_format`
    exists to provide.

  Go's client speaks only the OpenAI-compatible endpoint, so Rust's Anthropic-native
  path (no `response_format` field there; it forces a synthetic single tool call
  whose `input_schema` IS the schema) has nothing to mirror here and is called out
  in the source rather than silently omitted.

  11 new tests, mirroring the Rust reference's own wire assertions
  (`openai_request_carries_response_format_json_schema` and
  `no_response_format_is_omitted_from_the_wire`) one-for-one.

## 1.7.13

### Patch Changes

- 3c07e14: feat(ts,python,dotnet): ship a REAL OpenAI-compatible HTTP client, not just the mock

  Rust builds an `LlmClient` from `config.llm`; Go ships `GatewayClient`. TypeScript,
  Python and .NET shipped only `MockLlmProvider` — anyone pointing an engine at a live
  gateway had to hand-roll the client, and the one thing a hand-rolled client reliably
  gets wrong is the cost header. All three now ship one.

  - **TypeScript** — `createGatewayClient({ baseURL, apiKey })` (and `gatewayClientFrom(sdk)`
    to bring your own configured SDK client).
  - **Python** — `GatewayLlmProvider(base_url=…, api_key=…)` (or `client=…`).
  - **.NET** — `new GatewayChatClient(baseUrl, apiKey, model)`.

  TS and Python are thin adapters over the `openai` SDK, which both packages already
  depend on — the SDK owns the wire format, SSE framing, timeouts and connection
  retries. .NET has no OpenAI dependency (and the MEAI adapter hides the HTTP response
  anyway, which defeats the whole point), so it is a full `HttpClient` port of
  `go/core/openai.go` and adds no NuGet package.

  Two behaviours are the reason these are not one-liners:

  - **The cost header is read BEFORE the response body.** Per-request cost is reported
    only in a header, and on the streaming path those headers are unreachable once the
    SSE body is being consumed — the regression core#102 fixed in Rust. Each client
    parses them up front and carries the cost on the response (blocking) or a leading
    chunk (streaming, matching Go), so it folds into the turn even if the stream errors
    before usage arrives. The parser is the one core#121 already shipped per language —
    imported, not re-derived — so first-non-zero precedence and the absent-vs-zero
    distinction stay identical across all five engines.
  - **`metadata` is omitted when unset**, keeping the request byte-identical to a client
    without the field. Python additionally drops `tools: null` (the agent passes `None`
    for a toolless turn, and a literal null is not what "unset" means on the wire).

  `MockLlmProvider` remains the default/test seam and nothing constructs the real client
  for you, so an unwired consumer is unchanged. The one behaviour change: the TypeScript
  streaming loop now folds a chunk-carried gateway cost into `costUsd`, which previously
  had no way to reach it (Python's `run_stream` already did this).

  Tests are real round-trips against a local HTTP+SSE server in each language — TS 10,
  Python 12, .NET 13 — covering non-streaming content+usage, streaming deltas, the
  cost header landing on the turn, `metadata` present/absent on the wire, and an absent
  header falling back to local pricing rather than recording a bogus $0.

## 1.7.12

### Patch Changes

- 8a962f8: feat(go,ts,python,dotnet): port the project context loader (AGENTS.md / CLAUDE.md)

  Rust has had `context.rs` since pearl th-5002c4 — the loader that finds a project's
  agent-instruction file and hands the host a string to put in the system prompt. The
  four ports never had it, so an embedder in Go/TS/Python/.NET had to hand-roll the
  discovery walk (and get the precedence wrong).

  All four now mirror Rust exactly:

  - **Two layers, stacked.** The USER layer reads `~/.smooth/CONTEXT.md` →
    `AGENTS.md` → `CLAUDE.md` (first non-blank wins) and is prepended under a
    `## User context (~/.smooth)` heading. The PROJECT layer walks UP from the working
    directory, taking the first hit of `.smooth/CONTEXT.md` → `SMOOTH.md` → `AGENTS.md`
    → `CLAUDE.md` per directory. Either layer alone is enough — a bare CLAUDE.md with no
    user file still loads, which is the whole point of the fallback chain.
  - **`## File References` resolved inline.** `- [Label](path.md#fragment) — description`
    is read relative to the context file's own directory and appended in a
    `## Resolved File References` block. A `#fragment` extracts just that markdown
    section, matched on GitHub-style heading anchors, ending at the next
    same-or-higher-level heading.
  - **Absent files are a no-op.** Nothing found in either layer returns nothing
    (`nil` / `undefined` / `None` / `null`); an unreadable reference is skipped rather
    than failing the load. Behavior is unchanged for any project without these files.

  Like the Rust reference this is a **standalone loader, not a hook inside the agent
  loop** — the host calls it and decides what to do with the string, so no existing
  agent behavior changes. Public as `LoadProjectContext` (Go),
  `loadProjectContext` (TypeScript), `load_project_context` (Python) and
  `ProjectContext.Load` (.NET).

  Rust's 15 `context.rs` tests are ported one-for-one to each language (60 new tests):
  link parsing with/without fragment and description, section-scoped reference
  collection, anchor normalization, section extraction (found / to-EOF / not-found),
  the four precedence cases, the walk-up, and the raw-passthrough when a file carries
  no references.

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
