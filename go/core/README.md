<p align="center">
  <a href="https://smoo.ai"><img src="https://raw.githubusercontent.com/SmooAI/smooth-operator-core/main/.github/banner-go.png" alt="smooth-operator-core — The Go engine for orchestrated AI agents" width="100%" /></a>
</p>

<p align="center">
  <a href="https://smoo.ai/th"><img src="https://img.shields.io/badge/Smoo_AI-platform-00A6A6?style=for-the-badge&labelColor=020618" alt="Smoo AI"></a>
  <a href="https://github.com/SmooAI/smooth-operator-core/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-MIT-F49F0A?style=for-the-badge&labelColor=020618" alt="license"></a>
  <a href="https://lom.smoo.ai"><img src="https://img.shields.io/badge/hosted-lom.smoo.ai-FF6B6C?style=for-the-badge&labelColor=020618" alt="lom.smoo.ai"></a>
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/SmooAI/smooth-operator-core/go/core"><img src="https://img.shields.io/badge/pkg.go.dev-reference-00ADD8?style=flat-square&labelColor=020618" alt="pkg.go.dev"></a>
  <img src="https://img.shields.io/badge/Go-engine-00ADD8?style=flat-square&labelColor=020618" alt="Go engine">
</p>

---

> The Go sibling of the [Rust reference engine](https://github.com/SmooAI/smooth-operator-core). Agents, tools, knowledge/RAG, memory, checkpointing, human-in-the-loop, cost budgets, and workflows — as one embeddable package. It's the engine, not a notebook demo.

`github.com/SmooAI/smooth-operator-core/go/core` is the **native Go implementation** of the Smoo AI agent engine — the in-process observe→think→act loop that powers [**lom.smoo.ai**](https://lom.smoo.ai). It's a sibling of the [Rust reference engine](https://github.com/SmooAI/smooth-operator-core) and one of the [polyglot set](https://github.com/SmooAI/smooth-operator-core/blob/main/docs/Polyglot-Engines.md) (Rust, TypeScript, Python, Go, C#/.NET) whose behavior is held at parity by a shared eval suite.

It's a library, not a client to a remote server: it *is* the agent, running in your Go process. Every surface is covered by **fast, offline tests** built on a deterministic `MockLlmProvider`, so the loop is verified — not vibe-coded.

## Install

```bash
go get github.com/SmooAI/smooth-operator-core/go/core
```

The engine is the `core` package; idiomatic alias `core`.

## Quickstart

A complete agent — no credentials needed — using the deterministic mock provider the engine's own tests run on:

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

`NewSmoothAgent(client, options)` takes a `ChatClient` (the `MockLlmProvider` implements it — swap in any OpenAI-compatible client) and an `AgentOptions` struct. `Run(ctx, message, history)` — pass `nil` history for a fresh turn — returns `(AgentRunResponse, error)`; `res.Text` is the final answer.

## Features

The full parity surface — every engine in the [polyglot set](https://github.com/SmooAI/smooth-operator-core/blob/main/docs/Polyglot-Engines.md) ships it:

- **Agentic tool-calling loop** — observe→think→act, looping until the model answers.
- **Typed tools** — register tools the model can call, with parallel dispatch.
- **Knowledge / RAG + vectors** — ground the turn in retrieved documents.
- **Memory** — long-term entries recalled into context each turn.
- **Compaction** — a sliding-window token budget keeps the prompt under a ceiling.
- **Cost / budget** — per-model pricing, token + USD accounting, early stop on budget.
- **Checkpointing** — persist/resume a conversation via a checkpoint store.
- **Rerank** — rerank retrieved hits before injection (lexical reranker built in).
- **Sub-agents / delegation** — spawn child agents for sub-tasks.
- **Cast + clearance** — roles with per-role tool-access policy.
- **Human-in-the-loop gate** — require approval before designated tool calls run.
- **Conversation thread** — carry a conversation across multiple `Run` calls.
- **`LlmProvider` seam + `MockLlmProvider`** — inject any OpenAI-compatible client; the record/replay mock drives the offline tests.
- **Deferred tools + `tool_search`** — hide rarely-used tool schemas behind a meta-tool the model calls to promote the ones it needs.
- **Typed workflow graph** — a generic `Workflow[S]` node/edge engine alongside the agent loop.
- **Parallel tool calls** — dispatch ≥2 tool calls concurrently (transcript order preserved).
- **Retry / backoff** — retry transient model-call failures with exponential backoff.
- **Streaming** — stream incremental text, tool calls, and tool results as the turn runs.

## Streaming

`RunStream` is the streaming variant of `Run`: it yields incremental events — `text` deltas as the model produces them, each tool call before dispatch, each tool result after it finishes, and a terminal `done` event carrying the same response `Run` would have returned.

## Part of Smoo AI

`smooth-operator-core` is built and open-sourced by **[Smoo AI](https://smoo.ai)** — the AI-powered business platform with AI built into every product: CRM, customer support, campaigns, field service, observability, and developer tools.

- 🚀 **Smooth on the platform** — [smoo.ai/th](https://smoo.ai/th)
- 🧰 **More open source from Smoo AI** — [smoo.ai/open-source](https://smoo.ai/open-source)
- 🧩 **Run it hosted** — [lom.smoo.ai](https://lom.smoo.ai)

## Links

- [**lom.smoo.ai**](https://lom.smoo.ai) — run it hosted
- [smooth-operator-core](https://github.com/SmooAI/smooth-operator-core) — the polyglot engine repo
- [Polyglot Engines](https://github.com/SmooAI/smooth-operator-core/blob/main/docs/Polyglot-Engines.md) — install + hello-agent in all five languages
- [smoo.ai](https://smoo.ai) — the product · [smoo.ai/open-source](https://smoo.ai/open-source) — more open source

## License

MIT — see [LICENSE](https://github.com/SmooAI/smooth-operator-core/blob/main/LICENSE).

---

<p align="center">
  Built by <a href="https://smoo.ai"><strong>Smoo AI</strong></a> — AI built into every product.
</p>
