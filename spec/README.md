# `spec/` — the wire protocol (source of truth)

Language-neutral JSON Schemas for the smooth-operator WebSocket protocol. Every language client/service regenerates its types from here and validates against the shared conformance fixtures, so the protocol cannot drift between languages.

- `envelope.schema.json` — the action/event envelope
- `actions/` — client→server messages (`send_message`, `create_conversation_session`, …)
- `events/` — server→client messages (`stream_chunk`, `eventual_response`, …)
- `domain/` — `conversation`, `participant`, `message`, `session`, `checkpoint`
- `codegen/` — per-language generator config (TS, Go, .NET, Python)

See [`../docs/PROTOCOL.md`](../docs/PROTOCOL.md) for the design. Schemas are lifted and generalized from the smooai monorepo's `@smooai/realtime` package.
