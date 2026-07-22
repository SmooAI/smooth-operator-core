<p align="center">
  <a href="https://smoo.ai"><img src="https://raw.githubusercontent.com/SmooAI/smooth-operator-core/main/.github/banner-dotnet.png" alt="smooth-operator-core — The C# / .NET engine for orchestrated AI agents" width="100%" /></a>
</p>

<p align="center">
  <a href="https://smoo.ai/th"><img src="https://img.shields.io/badge/Smoo_AI-platform-00A6A6?style=for-the-badge&labelColor=020618" alt="Smoo AI"></a>
  <a href="https://github.com/SmooAI/smooth-operator-core/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-MIT-F49F0A?style=for-the-badge&labelColor=020618" alt="license"></a>
  <a href="https://lom.smoo.ai"><img src="https://img.shields.io/badge/hosted-lom.smoo.ai-FF6B6C?style=for-the-badge&labelColor=020618" alt="lom.smoo.ai"></a>
</p>

<p align="center">
  <a href="https://www.nuget.org/packages/SmooAI.SmoothOperator.Core"><img src="https://img.shields.io/nuget/v/SmooAI.SmoothOperator.Core?style=flat-square&color=00A6A6&labelColor=020618" alt="NuGet"></a>
  <img src="https://img.shields.io/badge/.NET-engine-512BD4?style=flat-square&labelColor=020618" alt=".NET engine">
</p>

---

> ### The agent brain you can point at production — right in your .NET process.
>
> Most agent frameworks hand the model a pile of tools and hope. This one gives you the loop **and the brakes**: draw hard lines the model can never cross, then let it run.

`SmooAI.SmoothOperator.Core` is the agent engine itself, in-process — an observe→think→act loop over any `IChatClient`, with typed tools (authored from ordinary C# methods via `AIFunctionFactory`), streaming, checkpointing, cost budgets, and a permission gate you control. Its API follows Microsoft.Extensions.AI naming, so it drops into an existing .NET AI stack. Not a client to a remote server: the agent *is* your process.

It's the native C# port of the [Rust reference engine](https://github.com/SmooAI/smooth-operator-core) — one of five siblings (Rust, TypeScript, Python, Go, C#/.NET) that share one wire spec and one eval suite. **The same agent brain, the same guarantees, wherever your stack already lives.** Every surface is covered by fast, offline tests on a deterministic `MockLlmProvider`, so the loop is verified — not vibe-coded.

## Install

```bash
dotnet add package SmooAI.SmoothOperator.Core
```

## Quickstart

A complete agent — no credentials needed — using the deterministic mock provider the engine's own tests run on:

A complete agent with one tool — the mock is scripted to call the tool, then answer. Author tools from ordinary C# methods with `AIFunctionFactory.Create`:

```csharp
using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

var getWeather = AIFunctionFactory.Create(
    (string city) => $"Weather in {city}: 72F, sunny",
    "get_weather",
    "Get the current weather for a city");

var provider = new MockLlmProvider()
    .PushToolCall("call_1", "get_weather", new Dictionary<string, object?> { ["city"] = "Tokyo" })
    .PushText("It's 72F and sunny in Tokyo.");

var options = new AgentOptions { Instructions = "You are a helpful assistant" };
options.Tools.Add(getWeather);
var agent = new SmoothAgent(provider, options);

var response = await agent.RunAsync("what's the weather in Tokyo?");
Console.WriteLine(response.Text);
```

`new SmoothAgent(chatClient, options)` takes an `IChatClient` (the `MockLlmProvider` implements it — swap in any OpenAI-compatible client) and an `AgentOptions`; tools are `AITool`s added to `options.Tools`. `await agent.RunAsync(...)` returns an `AgentRunResponse`; `response.Text` is the final assistant message.

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
- **Conversation thread** — carry a conversation across multiple `RunAsync` calls.
- **`LlmProvider` seam + `MockLlmProvider`** — inject any OpenAI-compatible client; the record/replay mock drives the offline tests.
- **Deferred tools + `tool_search`** — hide rarely-used tool schemas behind a meta-tool the model calls to promote the ones it needs.
- **Typed workflow graph** — a node/edge workflow engine alongside the agent loop.
- **Parallel tool calls** — dispatch ≥2 tool calls concurrently (transcript order preserved).
- **Retry / backoff** — retry transient model-call failures with exponential backoff.
- **Streaming** — stream incremental text, tool calls, and tool results as the turn runs.

## Permissions & deny-policy — lines the agent can't cross

This is what makes an agent safe to point at real infrastructure: **you** decide what it can never do, and no prompt or model mistake talks it out of that. Every tool call passes through a gate. `AutoMode` sets the posture — read-only calls **allow**, mutating calls **ask**, dangerous calls **deny** — and hard circuit-breakers (`rm -rf /`, credential paths, pipe-to-shell, dangerous domains) fire in every mode, `Bypass` included. Attach a `DenyPolicy` on top: declarative TOML rules for the lines you can name, semantic predicates for the ones you can't. A match is a hard deny no stored grant and no mode can waive.

```csharp
using Microsoft.Extensions.AI;
using SmooAI.SmoothOperator.Core;

// Declarative rules (TOML): never the prod AWS profile, never a prod host.
var policy = DenyPolicy.FromToml(@"
    schema_version = 1
    [bash]
    deny_patterns = [""aws * --profile prod""]
    [network]
    deny_hosts = [""*.prod.internal""]
").WithPredicate(new DenyDbWriter());

var options = new AgentOptions { Instructions = "You are a careful assistant" }
    .WithPermissionMode(AutoMode.Ask) // read allow · mutate ask · dangerous deny
    .WithDenyPolicy(policy);
options.Tools.Add(getWeather);
var agent = new SmoothAgent(provider, options);

// A predicate for what strings can't express — return a DenyReason to deny, null to allow.
sealed class DenyDbWriter : IDenyPredicate
{
    public DenyReason? Evaluate(FunctionCallContent call) =>
        call.Name == "db_query" && call.Arguments?.Values.Any(v => $"{v}".Contains("writer")) == true
            ? new DenyReason("DB writer endpoint is off-limits — reads go to the replica")
            : null;
}
```

## Streaming

`RunStreamingAsync` is the streaming variant of `RunAsync`: it yields incremental updates — text deltas as the model produces them, each tool call before dispatch, each tool result after it finishes, and a terminal update carrying the same response `RunAsync` would have returned.

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
