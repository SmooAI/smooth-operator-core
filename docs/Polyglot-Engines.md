# Polyglot Engines

`smooth-operator-core` ships the same agent engine in **five languages** — Rust, TypeScript, Python, Go, and C#/.NET. This page is the install + usage reference for all of them.

## What it is

`smooth-operator-core` is the polyglot **engine library**: an in-process, embeddable agentic loop (observe → think → act), the "LangGraph analog" of the SmooAI stack. You construct an agent, hand it an LLM client and some options, and call `run` — it grounds the turn in knowledge/memory, calls the model, dispatches any tool calls, feeds the results back, and loops until the model produces a final answer.

It is a library, not a service. There is no server, transport, or deployment here.

### Topology

- **`smooth-operator-core`** (this repo) = the polyglot ENGINE library — the in-process agentic loop you embed in your own process. Think "LangGraph."
- **[`smooth-operator`](https://github.com/SmooAI/smooth-operator)** (separate repo) = the SYSTEM / service that consumes the engine — server, transport, persistence, deploy. Think "Onyx."

The Rust crate is the reference implementation. The other four engines mirror its **behavior**, not its exact type shapes — parity is checked by an eval suite that every engine loads from ONE shared corpus, [`spec/evals/scenarios.json`](../spec/evals/scenarios.json), so idioms stay native to each language (snake_case in Python, `*Async` in C#, `error` returns in Go) while the observable behavior matches.

## Feature surface (the shared core, in all five engines)

Every engine supports the same core capabilities below. Beyond this shared core, the Rust reference carries surfaces still being ported (multimodal images, structured output, prompt caching, provider routing, the NarcHook secret/injection scanner, and the extension sandbox/integrity hardening) — if a capability matters to you in a non-Rust engine, check that language's package docs before assuming it. A real gateway LLM client is no longer one of them: all five now ship one, though the Rust client's provider quirks/routing remain ahead.

- **Agentic tool-calling loop** — observe → think → act, looping until the model answers.
- **In-memory + vector knowledge (RAG)** — ground the turn in retrieved documents.
- **Memory** — long-term entries recalled into context each turn.
- **Compaction** — sliding-window context-token budget keeps the prompt under a ceiling.
- **Project context loader** — stack the user's `~/.smooth/CONTEXT.md` above the nearest project `.smooth/CONTEXT.md` / `SMOOTH.md` / `AGENTS.md` / `CLAUDE.md` (walking up from the working directory), resolving any `## File References` inline. A standalone loader in every engine: the host decides whether to inject the result into the system prompt.
- **Cost / budget** — per-model pricing, token + USD accounting, early stop on budget.
- **Checkpointing** — persist/resume a conversation via a checkpoint store.
- **Rerank** — rerank retrieved hits before injection; a lexical reranker (query-term coverage normalized by document length) is built into all five.
- **Sub-agents / delegation** — spawn child agents for sub-tasks.
- **Cast** — roles + clearance (tool-access policy per role).
- **Human-in-the-loop gate** — require approval before designated tool calls run.
- **Conversation thread** — carry a conversation across multiple `run` calls.
- **`LlmProvider` seam + `MockLlmProvider`** — inject any OpenAI-compatible client; a deterministic record/replay mock drives the offline tests, and a shipped real HTTP client talks to a live gateway (see [Talking to a real gateway](#talking-to-a-real-gateway)).
- **Deferred tools + `tool_search`** — hide rarely-used tool schemas behind a built-in `tool_search` meta-tool the model calls to promote the ones it needs.
- **Typed workflow graph** — a node/edge workflow engine alongside the agent loop.
- **Parallel tool calls** — dispatch ≥2 tool calls concurrently (transcript order preserved).
- **Retry / backoff** — retry transient model-call failures with exponential backoff.
- **Streaming** — stream incremental text, tool calls, and tool results as the turn runs.

## Per-language install + hello agent

Each example constructs an agent with the **mock provider** (record/replay, no network) and runs one turn. The mock is the same deterministic seam the engine's offline tests use, so these examples run with zero credentials.

### Rust

Crate `smooai-smooth-operator-core` (lib `smooth_operator_core`), version **1.7.1**. The Rust engine names the agent `Agent` (configured with `AgentConfig`) and the mock `MockLlmClient`.

```bash
cargo add smooai-smooth-operator-core
cargo add tokio --features full
cargo add anyhow
```

```rust
use std::sync::Arc;
use smooth_operator_core::{Agent, AgentConfig, LlmConfig, ToolRegistry};
use smooth_operator_core::llm_provider::MockLlmClient;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let mock = MockLlmClient::new();
    mock.push_text("the answer is 42");

    let config = AgentConfig::new("agent", "You are a helpful assistant", LlmConfig::openrouter("fake-key"));
    let agent = Agent::new(config, ToolRegistry::new())
        .with_llm_provider(Arc::new(mock.clone()));

    let conversation = agent.run("what is the answer?").await?;
    println!("{}", conversation.last_assistant_content().unwrap_or(""));
    Ok(())
}
```

`with_llm_provider` injects the mock (any `Arc<dyn LlmProvider>`); without it, `run` builds a real `LlmClient` from `config.llm`. `run` returns a `Conversation`; `last_assistant_content()` is the final answer.

### TypeScript

npm `@smooai/smooth-operator-core`, version **1.7.1**.

```bash
npm install @smooai/smooth-operator-core
```

```ts
import { SmoothAgent, MockLlmProvider } from '@smooai/smooth-operator-core';

const provider = new MockLlmProvider().pushText('the answer is 42');
const agent = new SmoothAgent(provider, { instructions: 'You are a helpful assistant' });

const response = await agent.run('what is the answer?');
console.log(response.text);
```

`SmoothAgent`'s constructor takes a `ChatClientLike` (the `MockLlmProvider` implements it) and an `AgentOptions` object. `run` returns an `AgentRunResponse` whose `text` is the final answer.

### Python

PyPI `smooai-smooth-operator-core`, version **1.7.1**. `run` is async.

```bash
pip install smooai-smooth-operator-core
```

```python
import asyncio
from smooth_operator_core import SmoothAgent, AgentOptions, MockLlmProvider

async def main():
    provider = MockLlmProvider()
    provider.push_text("the answer is 42")

    agent = SmoothAgent(provider, AgentOptions(instructions="You are a helpful assistant"))
    result = await agent.run("what is the answer?")
    print(result.text)

asyncio.run(main())
```

`SmoothAgent(chat_client, options)` takes the provider and an `AgentOptions` dataclass (all fields default, so `AgentOptions()` is valid). `await agent.run(...)` returns an `AgentRunResponse`; `result.text` is the final answer. (`run_stream` is the streaming variant.)

### Go

Module `github.com/SmooAI/smooth-operator-core/go`; the engine is the `core` package at `…/go/core`. `Run` is context-aware and returns `(AgentRunResponse, error)`.

```bash
go get github.com/SmooAI/smooth-operator-core/go/core
```

```go
package main

import (
	"context"
	"fmt"

	core "github.com/SmooAI/smooth-operator-core/go/core"
)

func main() {
	provider := core.NewMockLlmProvider().PushText("the answer is 42")
	agent := core.NewSmoothAgent(provider, core.AgentOptions{Instructions: "You are a helpful assistant"})

	res, err := agent.Run(context.Background(), "what is the answer?", nil)
	if err != nil {
		panic(err)
	}
	fmt.Println(res.Text)
}
```

`NewSmoothAgent(client, options)` takes a `ChatClient` (the `MockLlmProvider` implements it) and an `AgentOptions` struct. `Run(ctx, message, history)` — pass `nil` history for a fresh turn — returns an `AgentRunResponse`; `res.Text` is the final answer. (`RunStream` is the streaming variant.)

### C# / .NET

NuGet `SmooAI.SmoothOperator.Core`, version **1.7.1**. `RunAsync` is async. The API follows Microsoft.Extensions.AI (`MAF`) naming.

```bash
dotnet add package SmooAI.SmoothOperator.Core
```

```csharp
using SmooAI.SmoothOperator.Core;

var provider = new MockLlmProvider().PushText("the answer is 42");
var agent = new SmoothAgent(provider, new AgentOptions { Instructions = "You are a helpful assistant" });

var response = await agent.RunAsync("what is the answer?");
Console.WriteLine(response.Text);
```

`new SmoothAgent(chatClient, options)` takes an `IChatClient` (the `MockLlmProvider` implements it) and an `AgentOptions`. `await agent.RunAsync(...)` returns an `AgentRunResponse`; `response.Text` is the final assistant message. (`RunStreamingAsync` is the streaming variant.)

## Talking to a real gateway

The examples above use the mock so they run with no credentials. Every engine also ships a **real** client for any OpenAI-compatible `/chat/completions` endpoint — the SmooAI gateway (`https://llm.smoo.ai/v1`), or anything that speaks the same wire. The mock stays the default/test seam; the real client is opt-in, so an unwired consumer behaves exactly as before.

| Engine     | Construct                                                              | Implementation                                                       |
| ---------- | ---------------------------------------------------------------------- | -------------------------------------------------------------------- |
| Rust       | built from `config.llm` when no provider is injected                    | native (`src/llm.rs`)                                                 |
| TypeScript | `createGatewayClient({ baseURL, apiKey })`                              | thin adapter over the `openai` SDK (already a dependency)             |
| Python     | `GatewayLlmProvider(base_url=…, api_key=…)`                             | thin adapter over the `openai` SDK (already a dependency)             |
| Go         | `core.NewGatewayClient(baseURL, apiKey)`                                | native `net/http` (`go/core/openai.go`)                               |
| C# / .NET  | `new GatewayChatClient(baseUrl, apiKey, model)`                         | native `HttpClient` — no added NuGet dependency                       |

All five do the same two things beyond the plain wire format, and both are easy to get wrong:

- **Read the gateway cost header before the body.** Per-request cost is reported ONLY in a response header, and on the **streaming** path those headers are unreachable once the SSE body is being consumed. Each client parses them up front and carries the cost forward (on the response when blocking; on a leading chunk when streaming), so it folds into the turn's `costUsd` even if the stream errors before usage arrives. Reading them after iterating the stream silently finds nothing — the regression fixed in Rust by core#102 and swept across the ports by core#121. The candidate list and its first-**non-zero** precedence is shared: `x-litellm-response-cost-margin-amount` → `-original` → `x-litellm-response-cost` → `x-response-cost` → `x-cost-usd`. A header that is present and reports `0` is NOT a recorded $0 — it falls through, and if nothing measures, the local pricing estimate is used instead.
- **Send `metadata` only when set.** The top-level OpenAI-compat `metadata` object (LiteLLM records it on spend logs) is omitted entirely when unset, keeping the request byte-identical to a client that never knew about the field.

## Streaming

The newest surface is streaming. Instead of `run` returning a single response, the stream variant yields incremental events: text deltas as the model produces them, each tool call before it is dispatched, each tool result after it finishes, and a single terminal `done` event carrying the same response `run` would have returned.

In TypeScript, `runStream` is an async generator over a `StreamEvent` tagged union (discriminated on `type`):

```ts
import { SmoothAgent, MockLlmProvider } from '@smooai/smooth-operator-core';

const provider = new MockLlmProvider().pushText('the answer is 42');
const agent = new SmoothAgent(provider, {});

for await (const event of agent.runStream('what is the answer?')) {
    switch (event.type) {
        case 'text':
            process.stdout.write(event.text);
            break;
        case 'tool_call':
            console.log(`\n[tool_call] ${event.name}(${event.arguments})`);
            break;
        case 'tool_result':
            console.log(`[tool_result] ${event.name} -> ${event.result}`);
            break;
        case 'done':
            console.log(`\n[done] ${event.response.text}`);
            break;
    }
}
```

`StreamEvent` is `{ type: 'text'; text } | { type: 'tool_call'; name; arguments } | { type: 'tool_result'; name; result } | { type: 'done'; response }`. `runStream` requires a streaming-capable client (`chat.completions.createStream`); the `MockLlmProvider` supplies one, replaying the same script as the non-streaming path. The other engines expose the same event sequence under their native idioms: Python's `run_stream`, Go's `RunStream`, C#'s `RunStreamingAsync`, and the Rust engine's event stream.

## Engine source

Each engine lives in its own directory:

- [`rust/smooth-operator-core`](../rust/smooth-operator-core) — the reference implementation.
- [`typescript/core`](../typescript/core)
- [`python/core`](../python/core)
- [`go/core`](../go/core)
- [`dotnet/core`](../dotnet/core)

## The eval suite

Parity across all five engines is checked by an **LLM-as-judge eval suite**. Every
engine loads the same corpus — [`spec/evals/scenarios.json`](../spec/evals/scenarios.json)
— and none of them define scenarios in code:

| Engine | Suite |
| --- | --- |
| Rust | `rust/smooth-operator-core/tests/evals.rs` |
| Go | `go/core/evals_test.go` |
| TypeScript | `typescript/core/test/evals.test.ts` |
| Python | `python/core/tests/test_evals.py` |
| .NET | `dotnet/core/tests/EvalTests.cs` |

The corpus has two tiers: **core** (behaviors every engine must clear, judged
against `aggregate_mean_threshold`) and **hard** (adversarial and
developer-experience probes on a lenient floor, so one hard miss is an improvement
target rather than a red build). Each scenario carries its knowledge-base
documents, user turns, ground truth, judge rubric, and an `intent` line saying
which failure it exists to catch.

**How it runs.** Two things happen at different times:

- **Every PR, offline, no credentials:** each language asserts the corpus it
  loaded matches the corpus file — same count, same ids — plus a ratchet on the
  scenario count. This is what stops an engine subsetting the suite or a scenario
  being quietly deleted. It needs no gateway and costs nothing.
- **Nightly, live:** [`evals-nightly.yml`](../.github/workflows/evals-nightly.yml)
  runs all five suites against the real gateway with `SMOOTH_AGENT_E2E=1` and the
  `SMOOAI_GATEWAY_KEY` repository secret, and scores each tier. A preflight job
  fails loudly if that secret is missing, because the suites self-skip without it
  and a silent skip otherwise looks exactly like a pass.

Running the live suite locally is the same gate in either direction — set both
`SMOOTH_AGENT_E2E=1` and `SMOOAI_GATEWAY_KEY`, or the suites skip.

> **History worth knowing.** These scenarios were hand-duplicated in four
> languages, each citing a canonical `rust/evals` directory that never existed in
> this repo — Rust, the reference engine, ran zero eval scenarios. The copies had
> already forked: .NET swapped `prompt_injection_in_kb` out of its core tier and
> grew a hard tier no other language had. Nothing had ever run in CI, because no
> workflow set the gating variables. The shared corpus plus the offline drift
> guard exist so that combination can't recur silently.
