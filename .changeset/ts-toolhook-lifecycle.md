---
"@smooai/smooth-operator-core": patch
---

feat(ts): ToolHook lifecycle for tool-call surveillance + redaction (polyglot parity with the Rust engine)

The TypeScript engine gains the Rust `ToolHook` lifecycle. New exported types
`ToolCall`, `ToolResult`, and `ToolHook` (with async `preCall(call)` — throw to
block — and `postCall(call, result)` where `result` is mutable so a hook can
redact `result.content` in place). `SmoothAgent` accepts hooks via
`AgentOptions.toolHooks` and a new `addHook(hook)` method (the registry seam),
running every hook's `preCall` before a tool executes and `postCall` after — the
mutation reaches the model/conversation. A throwing `postCall` is swallowed
(logged, not surfaced) so the redaction seam can never break a turn. Applied on
both `run` and `runStream`. This is the seam Narc / host-supplied surveillance
plug into, mirroring `smooth-operator-core/src/tool.rs`.
