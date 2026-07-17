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

> ### The agent brain you can point at production — right in your Go process.
>
> Most agent frameworks hand the model a pile of tools and hope. This one gives you the loop **and the brakes**: draw hard lines the model can never cross, then let it run.

`github.com/SmooAI/smooth-operator-core/go/core` is the agent engine itself, in-process — an observe→think→act loop over any OpenAI-compatible client, with typed tools, streaming, checkpointing, cost budgets, and a permission gate you control. Not a client to a remote server: the agent *is* your process.

It's the native Go port of the [Rust reference engine](https://github.com/SmooAI/smooth-operator-core) — one of five siblings (Rust, TypeScript, Python, Go, C#/.NET) that share one wire spec and one eval suite. **The same agent brain, the same guarantees, wherever your stack already lives.** Every surface is covered by fast, offline tests on a deterministic `MockLlmProvider`, so the loop is verified — not vibe-coded.

## Install

```bash
go get github.com/SmooAI/smooth-operator-core/go/core
```

The engine is the `core` package; idiomatic alias `core`.

## Quickstart

A complete agent — no credentials needed — using the deterministic mock provider the engine's own tests run on:

A complete agent with one tool — the mock is scripted to call the tool, then answer:

```go
package main

import (
	"context"
	"fmt"

	core "github.com/SmooAI/smooth-operator-core/go/core"
)

func main() {
	weather := core.FuncTool{
		ToolName: "get_weather",
		Desc:     "Get the current weather for a city",
		Params: map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []string{"city"},
		},
		Fn: func(ctx context.Context, args map[string]any) (string, error) {
			return fmt.Sprintf("Weather in %v: 72F, sunny", args["city"]), nil
		},
	}

	provider := core.NewMockLlmProvider().
		PushToolCall("call_1", "get_weather", `{"city":"Tokyo"}`).
		PushText("It's 72F and sunny in Tokyo.")

	agent := core.NewSmoothAgent(provider, core.AgentOptions{
		Instructions: "You are a helpful assistant",
		Tools:        []core.Tool{weather},
	})

	res, err := agent.Run(context.Background(), "what's the weather in Tokyo?", nil)
	if err != nil {
		panic(err)
	}
	fmt.Println(res.Text)
}
```

`NewSmoothAgent(client, options)` takes a `ChatClient` (the `MockLlmProvider` implements it — swap in any OpenAI-compatible client) and an `AgentOptions` struct. `FuncTool` wraps a function as a `Tool`. `Run(ctx, message, history)` — pass `nil` history for a fresh turn — returns `(AgentRunResponse, error)`; `res.Text` is the final answer.

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
- **Conversation thread** — carry a conversation across multiple `Run` calls.
- **`LlmProvider` seam + `MockLlmProvider`** — inject any OpenAI-compatible client; the record/replay mock drives the offline tests.
- **Deferred tools + `tool_search`** — hide rarely-used tool schemas behind a meta-tool the model calls to promote the ones it needs.
- **Typed workflow graph** — a generic `Workflow[S]` node/edge engine alongside the agent loop.
- **Parallel tool calls** — dispatch ≥2 tool calls concurrently (transcript order preserved).
- **Retry / backoff** — retry transient model-call failures with exponential backoff.
- **Streaming** — stream incremental text, tool calls, and tool results as the turn runs.

## Permissions & deny-policy — lines the agent can't cross

This is what makes an agent safe to point at real infrastructure: **you** decide what it can never do, and no prompt or model mistake talks it out of that. Every tool call passes through a gate. `AutoMode` sets the posture — read-only calls **allow**, mutating calls **ask**, dangerous calls **deny** — and hard circuit-breakers (`rm -rf /`, credential paths, pipe-to-shell, dangerous domains) fire in every mode, `AutoModeBypass` included. Attach a `DenyPolicy` on top: declarative TOML rules for the lines you can name, semantic predicates for the ones you can't. A match is a hard deny no stored grant and no mode can waive.

```go
// A DenyPredicate for what strings can't express — return (reason, true) to deny.
type denyDbWriter struct{}

func (denyDbWriter) Evaluate(name string, args map[string]any) (core.DenyReason, bool) {
	if name == "db_query" && strings.Contains(fmt.Sprint(args), "writer") {
		return core.NewDenyReason("DB writer endpoint is off-limits — reads go to the replica"), true
	}
	return core.DenyReason{}, false
}

// Declarative rules (TOML): never the prod AWS profile, never a prod host.
policy, err := core.DenyPolicyFromTOML(`
	schema_version = 1
	[bash]
	deny_patterns = ["aws * --profile prod"]
	[network]
	deny_hosts = ["*.prod.internal"]
`)
if err != nil {
	panic(err)
}
policy = policy.WithPredicate(denyDbWriter{})

mode := core.AutoModeAsk
agent := core.NewSmoothAgent(provider, core.AgentOptions{
	Instructions:   "You are a careful assistant",
	Tools:          []core.Tool{weather},
	PermissionMode: &mode, // read allow · mutate ask · dangerous deny
	DenyPolicy:     policy,
})
```

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
