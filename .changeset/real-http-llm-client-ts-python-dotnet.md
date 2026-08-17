---
"@smooai/smooth-operator-core": patch
---

feat(ts,python,dotnet): ship a REAL OpenAI-compatible HTTP client, not just the mock

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
