# Polyglot Engines

`smooth-operator-core` ships the same agent engine in **five languages** — Rust, TypeScript, Python, Go, and C#/.NET. This page is the install + usage reference for all of them.

## What it is

`smooth-operator-core` is the polyglot **engine library**: an in-process, embeddable agentic loop (observe → think → act), the "LangGraph analog" of the SmooAI stack. You construct an agent, hand it an LLM client and some options, and call `run` — it grounds the turn in knowledge/memory, calls the model, dispatches any tool calls, feeds the results back, and loops until the model produces a final answer.

It is a library, not a service. There is no server, transport, or deployment here.

### Topology

- **`smooth-operator-core`** (this repo) = the polyglot ENGINE library — the in-process agentic loop you embed in your own process. Think "LangGraph."
- **[`smooth-operator`](https://github.com/SmooAI/smooth-operator)** (separate repo) = the SYSTEM / service that consumes the engine — server, transport, persistence, deploy. Think "Onyx."

The Rust crate is the reference implementation. The other four engines mirror its **behavior**, not its exact type shapes — parity is enforced by a **shared eval suite** (the same scenarios run against every engine), so idioms stay native to each language (snake_case in Python, `*Async` in C#, `error` returns in Go) while the observable behavior matches.

## Feature surface (at parity across all five engines)

Every engine supports the same capabilities:

- **Agentic tool-calling loop** — observe → think → act, looping until the model answers.
- **In-memory + vector knowledge (RAG)** — ground the turn in retrieved documents.
- **Memory** — long-term entries recalled into context each turn.
- **Compaction** — sliding-window context-token budget keeps the prompt under a ceiling.
- **Cost / budget** — per-model pricing, token + USD accounting, early stop on budget.
- **Checkpointing** — persist/resume a conversation via a checkpoint store.
- **Rerank** — rerank retrieved hits before injection (lexical reranker built in).
- **Sub-agents / delegation** — spawn child agents for sub-tasks.
- **Cast** — roles + clearance (tool-access policy per role).
- **Human-in-the-loop gate** — require approval before designated tool calls run.
- **Conversation thread** — carry a conversation across multiple `run` calls.
- **`LlmProvider` seam + `MockLlmProvider`** — inject any OpenAI-compatible client; a deterministic record/replay mock drives the offline tests.
- **Deferred tools + `tool_search`** — hide rarely-used tool schemas behind a built-in `tool_search` meta-tool the model calls to promote the ones it needs.
- **Typed workflow graph** — a node/edge workflow engine alongside the agent loop.
- **Parallel tool calls** — dispatch ≥2 tool calls concurrently (transcript order preserved).
- **Retry / backoff** — retry transient model-call failures with exponential backoff.
- **Streaming** — stream incremental text, tool calls, and tool results as the turn runs.

## Per-language install + hello agent

Each example constructs an agent with the **mock provider** (record/replay, no network) and runs one turn. The mock is the same deterministic seam the engine's offline tests use, so these examples run with zero credentials.

### Rust

Crate `smooai-smooth-operator-core` (lib `smooth_operator_core`), version **0.14.0**. The Rust engine names the agent `Agent` (configured with `AgentConfig`) and the mock `MockLlmClient`.

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

npm `@smooai/smooth-operator-core`, version **0.1.0**.

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

PyPI `smooai-smooth-operator-core`, version **1.3.0**. `run` is async.

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

NuGet `SmooAI.SmoothOperator.Core`, version **1.3.0**. `RunAsync` is async. The API follows Microsoft.Extensions.AI (`MAF`) naming.

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

Parity across all five is enforced by a shared **eval suite** — the same behavioral scenarios run against every engine — so the engines stay behavior-compatible even where their type shapes differ.
