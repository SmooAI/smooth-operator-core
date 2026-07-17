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

> ### The agent brain you can point at production — right in your Python process.
>
> Most agent frameworks hand the model a pile of tools and hope. This one gives you the loop **and the brakes**: draw hard lines the model can never cross, then let it run.

`smooai-smooth-operator-core` is the agent engine itself, in-process — an observe→think→act loop over any OpenAI-compatible client, with typed tools, streaming, checkpointing, cost budgets, and a permission gate you control. Not a client to a remote server: the agent *is* your process.

It's the native Python port of the [Rust reference engine](https://github.com/SmooAI/smooth-operator-core) — one of five siblings (Rust, TypeScript, Python, Go, C#/.NET) that share one wire spec and one eval suite. **The same agent brain, the same guarantees, wherever your stack already lives.** Every surface is covered by fast, offline tests on a deterministic `MockLlmProvider`, so the loop is verified — not vibe-coded.

## Install

```bash
pip install smooai-smooth-operator-core
```

Import as `smooth_operator_core`.

## Quickstart

A complete agent — no credentials needed — using the deterministic mock provider the engine's own tests run on:

A complete agent with one tool — the mock is scripted to call the tool, then answer:

```python
import asyncio
import json
from smooth_operator_core import SmoothAgent, AgentOptions, FunctionTool, MockLlmProvider

async def get_weather(args):
    return f"Weather in {args['city']}: 72F, sunny"

async def main():
    weather = FunctionTool(
        name="get_weather",
        description="Get the current weather for a city",
        parameters={"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]},
        func=get_weather,
    )

    provider = MockLlmProvider()
    provider.push_tool_call("call_1", "get_weather", json.dumps({"city": "Tokyo"}))
    provider.push_text("It's 72F and sunny in Tokyo.")

    agent = SmoothAgent(provider, AgentOptions(instructions="You are a helpful assistant", tools=[weather]))
    result = await agent.run("what's the weather in Tokyo?")
    print(result.text)

asyncio.run(main())
```

`SmoothAgent(chat_client, options)` takes the provider (the `MockLlmProvider` — swap in any OpenAI-compatible client) and an `AgentOptions` dataclass (all fields default, so `AgentOptions()` is valid). `FunctionTool` wraps an async function as a tool. `await agent.run(...)` returns an `AgentRunResponse`; `result.text` is the final answer.

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
- **Permissions + deny-policy** — a tool-call gate (`AutoMode`: ask / accept-edits / deny-unmatched / bypass) with hard circuit-breakers (`rm -rf /`, credential paths, pipe-to-shell, dangerous domains), a persisted allow-list, and a consumer `DenyPolicy` — declarative TOML rules plus semantic predicates for what strings can't express.
- **Human-in-the-loop gate** — require approval before designated tool calls run.
- **Conversation thread** — `SmoothAgentThread` carries a conversation across multiple `run` calls.
- **`LlmProvider` seam + `MockLlmProvider`** — inject any OpenAI-compatible client; the record/replay mock drives the offline tests.
- **Deferred tools + `tool_search`** — hide rarely-used tool schemas behind a meta-tool the model calls to promote the ones it needs.
- **Typed workflow graph** — a node/edge workflow engine alongside the agent loop.
- **Parallel tool calls** — dispatch ≥2 tool calls concurrently (transcript order preserved).
- **Retry / backoff** — retry transient model-call failures with exponential backoff.
- **Streaming** — stream incremental text, tool calls, and tool results as the turn runs.

## Permissions & deny-policy — lines the agent can't cross

This is what makes an agent safe to point at real infrastructure: **you** decide what it can never do, and no prompt or model mistake talks it out of that. Every tool call passes through a gate. `AutoMode` sets the posture — read-only calls **allow**, mutating calls **ask**, dangerous calls **deny** — and hard circuit-breakers (`rm -rf /`, credential paths, pipe-to-shell, dangerous domains) fire in every mode, `BYPASS` included. Attach a `DenyPolicy` on top: declarative TOML rules for the lines you can name, semantic predicates for the ones you can't. A match is a hard deny no stored grant and no mode can waive.

```python
from smooth_operator_core import (
    SmoothAgent, AgentOptions, AutoMode, DenyPolicy, DenyPredicate, DenyReason,
)

# Declarative rules (TOML): never the prod AWS profile, never a prod host.
policy = DenyPolicy.from_toml(
    """
    schema_version = 1
    [bash]
    deny_patterns = ["aws * --profile prod"]
    [network]
    deny_hosts = ["*.prod.internal"]
    """
)

# Predicate for what strings can't express — return a DenyReason to deny, None to allow.
class DenyDbWriter(DenyPredicate):
    def evaluate(self, call):
        if call.name == "db_query" and "writer" in str(call.arguments):
            return DenyReason.new("DB writer endpoint is off-limits — reads go to the replica")
        return None

agent = SmoothAgent(
    provider,
    AgentOptions(
        instructions="You are a careful assistant",
        tools=[weather],
        permission_mode=AutoMode.ASK,  # read allow · mutate ask · dangerous deny
        deny_policy=policy.with_predicate(DenyDbWriter()),
    ),
)
```

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
