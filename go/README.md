<p align="center"><img src="../assets/smooth-logo.svg" alt="Smooth" width="360" /></p>

<p align="center"><strong>smooth-operator — Go client.</strong> An idiomatic, transport-agnostic, race-clean Go client for the smooth-operator protocol.</p>

<p align="center">
  <a href="../LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="MIT License" /></a>
  <img src="https://img.shields.io/badge/tests-26%20passing-success" alt="26 tests passing" />
  <img src="https://img.shields.io/badge/serverless%20%C2%B7%20polyglot%20%C2%B7%20TDD-6f42c1" alt="serverless · polyglot · TDD" />
  <a href="https://lom.smoo.ai"><img src="https://img.shields.io/badge/hosted-lom.smoo.ai-0aa" alt="lom.smoo.ai" /></a>
</p>

---

## What is this?

The **native Go client** for the [smooth-operator](../spec/README.md) WebSocket protocol. Wire types are generated from the language-neutral JSON Schemas in [`../spec/`](../spec) (committed, so you don't need the generator); the ergonomic layer gives you typed `As*` event accessors and a streaming `MessageTurn` that's both range-able and awaitable.

---

## 30-second quickstart

```bash
go get github.com/SmooAI/smooth-operator-core/go/protocol
```

```go
package main

import (
	"context"
	"fmt"

	"github.com/SmooAI/smooth-operator-core/go/protocol"
)

func main() {
	ctx := context.Background()

	c, _ := protocol.New(protocol.Options{
		Transport: protocol.NewWebSocketTransport("ws://127.0.0.1:8787/ws", nil),
	})
	_ = c.Connect(ctx)
	defer c.Close()

	sess, _ := c.CreateConversationSession(ctx, protocol.CreateConversationSessionParams{
		AgentID:  "11111111-1111-1111-1111-111111111111",
		UserName: "Alice",
	})

	turn := c.SendMessage(protocol.SendMessageParams{SessionID: sess.SessionID, Message: "How long is your return window?"})
	final, _ := turn.Wait(ctx)
	fmt.Println("messageId:", final.Data.Data.MessageID)
}
```

(Point the transport at your own [`smooth-operator-server`](../rust/README.md) or the hosted endpoint.)

---

## Watch it stream

`SendMessage` returns a `MessageTurn` — range over `Events()` for live tokens, then `Wait()` for the authoritative terminal response. Go has no sum types, so a `ServerEvent` carries the common envelope fields plus the raw frame; switch on `Type` and call the matching `As*` accessor.

```go
turn := c.SendMessage(protocol.SendMessageParams{SessionID: sess.SessionID, Message: "Where's my order?"})

for ev := range turn.Events() {
	switch ev.Type {
	case protocol.EventStreamToken:
		tok, _ := ev.AsStreamToken()
		fmt.Print(tok.Data.Token)                 // tokens, live
	case protocol.EventStreamChunk:
		chunk, _ := ev.AsStreamChunk()
		fmt.Printf("\n  ↳ node: %s\n", chunk.Node) // workflow node boundary
	case protocol.EventWriteConfirmationRequired:
		// HITL: approve and the resumed stream flows back into this same turn.
		c.ConfirmToolAction(protocol.ConfirmToolActionParams{
			SessionID: sess.SessionID, RequestID: turn.RequestID(), Approved: true,
		})
	}
}

final, _ := turn.Wait(ctx)
fmt.Println("\nmessageId:", final.Data.Data.MessageID)
```

```mermaid
%%{init: {'theme':'base','themeVariables':{'background':'#020618','primaryColor':'#0b1426','primaryTextColor':'#e6edf6','primaryBorderColor':'#2b3a52','lineColor':'#7c8aa0','actorBkg':'#0b1426','actorBorder':'#2b3a52','actorTextColor':'#e6edf6','signalColor':'#7c8aa0','signalTextColor':'#e6edf6','noteBkgColor':'#f49f0a','noteTextColor':'#1a0f00','noteBorderColor':'#ff6b6c','fontFamily':'ui-sans-serif, system-ui, sans-serif'}}}%%
sequenceDiagram
  participant App
  participant C as protocol.Client
  participant S as Service
  App->>C: SendMessage(...)
  C->>S: { action: send_message }
  S-->>C: immediate_response (202)
  S-->>C: stream_token / stream_chunk …
  S-->>C: eventual_response (200)
  C-->>App: range Events() · Wait() returns final
```

---

## Layout

| File | Purpose |
| --- | --- |
| `protocol/types_gen.go` | Generated wire types (one struct per schema / `$def`). **Do not edit.** |
| `protocol/events.go` | Ergonomic `ServerEvent` discrimination + typed `As*` accessors. |
| `protocol/transport.go` | `Transport` interface + default `coder/websocket` implementation. |
| `protocol/client.go` | `Client` with the action methods. |
| `protocol/turn.go` | `MessageTurn` (streaming events + awaitable terminal) + `ProtocolError`. |
| `protocol/validate.go` | Optional runtime JSON Schema validation against `../spec/`. |

The `Transport` interface is mockable, so the test suite drives real client code (correlation, event discrimination, HITL routing) without a network.

---

## Polyglot — one spec, five clients

```mermaid
%%{init: {'theme':'base','themeVariables':{'background':'#020618','primaryColor':'#0b1426','primaryTextColor':'#e6edf6','primaryBorderColor':'#2b3a52','lineColor':'#7c8aa0','secondaryColor':'#0b1426','tertiaryColor':'#0b1426','fontFamily':'ui-sans-serif, system-ui, sans-serif','clusterBkg':'#0b1426','clusterBorder':'#22304a'}}}%%
flowchart LR
  SPEC["spec/ (JSON Schema)"] --> GO["Go<br/>github.com/SmooAI/smooth-operator-core/go"]
  SPEC --> TS["TypeScript"]
  SPEC --> NET[".NET (+ MEAI IChatClient facade)"]
  SPEC --> PY["Python"]
  SPEC --> RS["Rust"]
```

---

## Test-driven by default

> **Nothing here is vibe-coded — it's verified against a real LLM gateway.**

```mermaid
%%{init: {'theme':'base','themeVariables':{'background':'#020618','primaryColor':'#0b1426','primaryTextColor':'#e6edf6','primaryBorderColor':'#2b3a52','lineColor':'#7c8aa0','secondaryColor':'#0b1426','tertiaryColor':'#0b1426','fontFamily':'ui-sans-serif, system-ui, sans-serif','clusterBkg':'#0b1426','clusterBorder':'#22304a'}}}%%
flowchart TD
  J["🎯 LLM-as-judge quality evals (Rust harness)"]
  E["🌐 Live cross-language E2E — this client boots the real server + drives a real claude-haiku-4-5 turn"]
  C["🧪 Conformance fixtures (shared across all 5 clients)"]
  U["⚡ Unit tests (event discrimination, transport, correlation) — race-clean"]
  J --> E --> C --> U
```

**26 tests, race-clean** (`go test -race`). In the live cross-language E2E ([`e2e_live_test.go`](e2e_live_test.go)), this client boots a real `smooth-operator-server` subprocess (KB seeded) and drives a real `claude-haiku-4-5` turn over WebSocket — asserting ≥1 streamed event, a knowledge-grounded "17", and per-session memory.

**The proof story:** an LLM-as-judge scored a multi-turn answer **1/5** (the runtime forgot turn 1's context); the failing eval drove a per-session-memory fix; **it now scores 5/5** — a regression a substring test would have missed. See [`docs/EVALS.md`](../docs/EVALS.md).

Live tests are **gated, never silently skipped** — they run with `SMOOTH_AGENT_E2E=1` + `SMOOAI_GATEWAY_KEY` and skip cleanly otherwise.

```bash
go test -race ./...                                   # no creds
SMOOTH_AGENT_E2E=1 go test -race ./... -run Live      # live cross-language E2E
```

## Regenerating types

Generated with [`go-jsonschema`](https://github.com/atombender/go-jsonschema) (pure Go, offline). `--only-models` is intentional: plain structs with no generated enum validation, so the client tolerates forward-compatible wire values and the conformance fixtures round-trip cleanly. Full command in the original spec; run it after any `../spec` change.

## Smoo-powered or bring-your-own

Point the transport at the hosted **[lom.smoo.ai](https://lom.smoo.ai)** endpoint, or at your own self-hosted `smooth-operator-server` — same protocol, same client.

## License

MIT © 2026 Smoo AI
