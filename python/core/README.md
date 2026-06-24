<p align="center">
  <a href="https://smoo.ai"><img src="https://raw.githubusercontent.com/SmooAI/smooth-operator-core/main/.github/banner-python.png" alt="smooth-operator-core — The Python engine for orchestrated AI agents" width="100%" /></a>
</p>

<p align="center">
  <a href="https://smoo.ai/th"><img src="https://img.shields.io/badge/Smoo_AI-platform-00A6A6?style=for-the-badge&labelColor=020618" alt="Smoo AI"></a>
  <a href="https://github.com/SmooAI/smooth-operator-core/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-MIT-F49F0A?style=for-the-badge&labelColor=020618" alt="license"></a>
  <a href="https://lom.smoo.ai"><img src="https://img.shields.io/badge/hosted-lom.smoo.ai-FF6B6C?style=for-the-badge&labelColor=020618" alt="lom.smoo.ai"></a>
</p>

<p align="center">
  <a href="https://pypi.org/project/smooai-smooth-operator-core/"><img src="https://img.shields.io/pypi/v/smooai-smooth-operator-core?style=flat-square&color=00A6A6&labelColor=020618" alt="PyPI"></a>
  <img src="https://img.shields.io/badge/Python-engine-3776AB?style=flat-square&labelColor=020618" alt="Python engine">
</p>

---

> The Python sibling of the [Rust reference engine](https://github.com/SmooAI/smooth-operator-core). Agents, tools, knowledge/RAG, memory, checkpointing, human-in-the-loop, cost budgets, and workflows — as one embeddable package. It's the engine, not a notebook demo.

`smooai-smooth-operator-core` is the **native Python implementation** of the Smoo AI agent engine — the in-process observe→think→act loop that powers [**lom.smoo.ai**](https://lom.smoo.ai). It's a sibling of the [Rust reference engine](https://github.com/SmooAI/smooth-operator-core) and one of the [polyglot set](https://github.com/SmooAI/smooth-operator-core/blob/main/docs/Polyglot-Engines.md) (Rust, TypeScript, Python, Go, C#/.NET) whose behavior is held at parity by a shared eval suite.

It's a library, not a client to a remote server: it *is* the agent, running in your Python process. Every surface is covered by **fast, offline tests** built on a deterministic `MockLlmProvider`, so the loop is verified — not vibe-coded.

## Install

```bash
pip install smooai-smooth-operator-core
```

Import as `smooth_operator_core`.

## Quickstart

A complete agent — no credentials needed — using the deterministic mock provider the engine's own tests run on:

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

`SmoothAgent(chat_client, options)` takes the provider (the `MockLlmProvider` — swap in any OpenAI-compatible client) and an `AgentOptions` dataclass (all fields default, so `AgentOptions()` is valid). `await agent.run(...)` returns an `AgentRunResponse`; `result.text` is the final answer.

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
- **Conversation thread** — `SmoothAgentThread` carries a conversation across multiple `run` calls.
- **`LlmProvider` seam + `MockLlmProvider`** — inject any OpenAI-compatible client; the record/replay mock drives the offline tests.
- **Deferred tools + `tool_search`** — hide rarely-used tool schemas behind a meta-tool the model calls to promote the ones it needs.
- **Typed workflow graph** — a node/edge workflow engine alongside the agent loop.
- **Parallel tool calls** — dispatch ≥2 tool calls concurrently (transcript order preserved).
- **Retry / backoff** — retry transient model-call failures with exponential backoff.
- **Streaming** — stream incremental text, tool calls, and tool results as the turn runs.

## Streaming

`run_stream` is the async streaming variant of `run`: it yields incremental events — `text` deltas as the model produces them, each tool call before dispatch, each tool result after it finishes, and a terminal `done` event carrying the same response `run` would have returned.

```python
async for event in agent.run_stream("what is the answer?"):
    if event.type == "text":
        print(event.text, end="")
    elif event.type == "done":
        print(f"\n{event.response.text}")
```

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
