---
"@smooai/smooth-operator-core": patch
---

fix: compaction no longer orphans tool results; sequential tool calls get a deadline; LLM client survives stale sockets (SMOODEV-3704, SMOODEV-3705)

**Compaction orphaned parallel tool results (Rust, .NET).** Rust's `SnipToolResults` /
`Summarize` cut history at `len - keep_recent`. When that cut landed inside a group of
parallel tool calls, the assistant message was dropped but the later results were kept,
and the gateway rejected the next request with `400 No tool call found for function call
output with call_id ...`, which killed the turn. `context_window()` had the same flaw when
it trimmed oldest-first through a group. Now:

- the compaction boundary moves back to the start of the assistant/tool group it lands in;
- `Conversation::compact` ends with the new `Conversation::sanitize_tool_pairs()`, which
  drops orphan or duplicate tool results and strips assistant tool calls that have no
  result. The agent loop also runs it before every LLM call;
- `context_window()` passes its trimmed window through the new
  `conversation::well_formed_tool_pairs`.

.NET's sliding window now drops an assistant tool call together with its results. The
Go, TypeScript and Python windows already trimmed leading tool results; they get
parallel-group regression tests.

**Per-tool deadline on the sequential path (Rust).** `ToolRegistry::execute` (the
default, non-parallel path) had no timeout, so a hung tool stalled the turn forever.
Each call now has a deadline (`DEFAULT_TOOL_TIMEOUT`, 120s). When it expires, the call
returns an error result and the turn goes on. You can override the deadline in three
ways:

- per registry, with `ToolRegistry::with_tool_timeout`;
- per tool, by implementing the new `Tool::timeout()`;
- for a tool the host doesn't own, with `ToolRegistry::set_tool_timeout(name, ..)`.

`NO_TOOL_TIMEOUT` disables the deadline. Sub-agent tools (`delegate`,
`send_sidekick`) use it. On both paths the deadline now covers only the tool's
`execute`, so time spent waiting for a human in a `pre_call` hook doesn't count.
**Hosts with long-running tools (shell, builds) should set an override.**

**LLM client (Rust).** Idle pooled connections now expire after 30s
(`pool_idle_timeout`), so a socket the gateway has already closed isn't reused. The
non-streaming `chat` (OpenAI-compatible and Anthropic) now retries transient transport
send errors, the way `chat_stream` already did, instead of failing the turn.
