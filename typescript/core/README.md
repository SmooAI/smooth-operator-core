<p align="center">
  <a href="https://smoo.ai"><img src="https://raw.githubusercontent.com/SmooAI/smooth-operator-core/main/.github/banner-typescript.png" alt="smooth-operator-core — The TypeScript engine for orchestrated AI agents" width="100%" /></a>
</p>

<p align="center">
  <a href="https://smoo.ai/th"><img src="https://img.shields.io/badge/Smoo_AI-platform-00A6A6?style=for-the-badge&labelColor=020618" alt="Smoo AI"></a>
  <a href="https://github.com/SmooAI/smooth-operator-core/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-MIT-F49F0A?style=for-the-badge&labelColor=020618" alt="license"></a>
  <a href="https://lom.smoo.ai"><img src="https://img.shields.io/badge/hosted-lom.smoo.ai-FF6B6C?style=for-the-badge&labelColor=020618" alt="lom.smoo.ai"></a>
</p>

<p align="center">
  <a href="https://www.npmjs.com/package/@smooai/smooth-operator-core"><img src="https://img.shields.io/npm/v/@smooai/smooth-operator-core?style=flat-square&color=00A6A6&labelColor=020618" alt="npm"></a>
  <img src="https://img.shields.io/badge/TypeScript-engine-3178C6?style=flat-square&labelColor=020618" alt="TypeScript engine">
</p>

---

> The TypeScript sibling of the [Rust reference engine](https://github.com/SmooAI/smooth-operator-core). Agents, tools, knowledge/RAG, memory, checkpointing, human-in-the-loop, cost budgets, and workflows — as one embeddable npm package. It's the engine, not a notebook demo.

`@smooai/smooth-operator-core` is the **native TypeScript implementation** of the Smoo AI agent engine — the in-process observe→think→act loop that powers [**lom.smoo.ai**](https://lom.smoo.ai). It's a sibling of the [Rust reference engine](https://github.com/SmooAI/smooth-operator-core) and one of the [polyglot set](https://github.com/SmooAI/smooth-operator-core/blob/main/docs/Polyglot-Engines.md) (Rust, TypeScript, Python, Go, C#/.NET) whose behavior is held at parity by a shared eval suite.

It's a library, not a client to a remote server: it *is* the agent, running in your Node process. Every surface is covered by **fast, offline tests** built on a deterministic `MockLlmProvider`, so the loop is verified — not vibe-coded.

## Install

```bash
npm install @smooai/smooth-operator-core
```

## Quickstart

A complete agent — no credentials needed — using the deterministic mock provider the engine's own tests run on:

```ts
import { SmoothAgent, MockLlmProvider } from '@smooai/smooth-operator-core';

const provider = new MockLlmProvider().pushText('the answer is 42');
const agent = new SmoothAgent(provider, { instructions: 'You are a helpful assistant' });

const response = await agent.run('what is the answer?');
console.log(response.text);
```

`SmoothAgent`'s constructor takes a `ChatClientLike` (the `MockLlmProvider` implements it — swap in any OpenAI-compatible client) and an `AgentOptions` object. `run` returns an `AgentRunResponse` whose `text` is the final answer.

## Features

The full parity surface — every engine in the [polyglot set](https://github.com/SmooAI/smooth-operator-core/blob/main/docs/Polyglot-Engines.md) ships it:

- **Agentic tool-calling loop** — observe→think→act, looping until the model answers.
- **Typed tools** — register `Tool`s the model can call, with parallel dispatch.
- **Knowledge / RAG + vectors** — `InMemoryKnowledge` / `VectorKnowledge` ground the turn in retrieved documents.
- **Memory** — `InMemoryMemory` recalls long-term entries into context each turn.
- **Compaction** — a sliding-window token budget keeps the prompt under a ceiling.
- **Cost / budget** — `CostTracker` + `CostBudget` with per-model pricing and early stop.
- **Checkpointing** — `InMemoryCheckpointStore` (and the `CheckpointStore` seam) persist/resume a conversation.
- **Rerank** — `LexicalReranker` reranks retrieved hits before injection.
- **Sub-agents / delegation** — `delegateTool` spawns child agents for sub-tasks.
- **Cast + clearance** — `Cast`, `Clearance`, `makeRole` for per-role tool-access policy.
- **Human-in-the-loop gate** — `HumanGate` requires approval before designated tool calls run.
- **Conversation thread** — `SmoothAgentThread` carries a conversation across multiple `run` calls.
- **`LlmProvider` seam + `MockLlmProvider`** — inject any OpenAI-compatible client; the record/replay mock drives the offline tests.
- **Deferred tools + `tool_search`** — `ToolSearch` hides rarely-used tool schemas behind a meta-tool the model calls to promote the ones it needs.
- **Typed workflow graph** — `Workflow` with typed nodes/edges, alongside the agent loop.
- **Parallel tool calls** — dispatch ≥2 tool calls concurrently (transcript order preserved).
- **Retry / backoff** — retry transient model-call failures with exponential backoff.
- **Streaming** — stream incremental text, tool calls, and tool results as the turn runs.

## Streaming

`runStream` is an async generator over a `StreamEvent` tagged union (discriminated on `type`): `text` deltas as the model produces them, each `tool_call` before dispatch, each `tool_result` after it finishes, and a terminal `done` event carrying the same response `run` would have returned.

```ts
for await (const event of agent.runStream('what is the answer?')) {
    if (event.type === 'text') process.stdout.write(event.text);
    if (event.type === 'done') console.log(`\n${event.response.text}`);
}
```

`runStream` requires a streaming-capable client (`chat.completions.createStream`); the `MockLlmProvider` supplies one, replaying the same script as the non-streaming path.

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
