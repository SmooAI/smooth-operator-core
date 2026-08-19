---
"@smooai/smooth-operator-core": patch
---

fix(python): three defects on the Python engine's streaming path

All three lived only in `SmoothAgent.run_stream`. The non-streaming `run` was
correct in every case, and Rust runs the same logic on both of its paths — which
is precisely why these stayed invisible: `run_stream` is the path the polyglot
servers, the TUI, and every real UI actually drive.

- **The SEP `tool_call` hook was never folded in.** Streaming dispatched tools
  directly, so an extension that `Block`s `payments.refund` was honored
  non-streaming and IGNORED streaming, and `Modify` argument rewrites (redaction,
  scoping) were silently dropped. Both loops now fold through the one
  `_sep_tool_call_plan`, which takes either tool-call shape (the SDK object or the
  dict reassembled from stream deltas). The emitted `ToolCallEvent` carries the
  PLANNED arguments, so a rewrite that redacts a secret does not leak through the
  UI event either — matching what Rust emits.

- **The model stream was never closed on early abandon.** A bare `async for` with
  no close meant a WS client disconnecting mid-answer unwound the generator on
  `GeneratorExit` and left the openai `AsyncStream`'s httpx response open until
  GC; under load the connection pool exhausted and later turns blocked on
  acquisition. The iteration is now wrapped so the stream is released on every
  exit, via `close()` (the openai SDK) or `aclose()` (a plain async generator).

- **An abandoned turn checkpointed a torn conversation.** The `finally` runs on
  `GeneratorExit` too, so abandoning between a tool call and its result persisted
  an assistant message with `tool_calls` and no tool replies. Every provider
  rejects that ("an assistant message with tool_calls must be followed by tool
  messages"), and since each retry reloaded the same checkpoint the conversation
  was permanently wedged. Persistence (checkpoint and thread alike, on both
  loops) now drops a conversation left mid-tool-chain, so the store keeps its last
  good state and the next turn resumes from that — the invariant Rust gets from
  checkpointing only at well-formed points.
