---
'@smooai/smooth-operator-core': minor
---

Make `ToolHook::post_call` a redaction seam and have `NarcHook` redact leaked secrets.

`post_call` now takes `&mut ToolResult` instead of `&ToolResult`, so a hook can
rewrite a tool result's `content` in place and the mutation is what the caller —
and therefore the LLM/conversation and every downstream consumer — actually
sees. The default trait impl remains a no-op; `ToolRegistry::execute` and
`execute_single` pass the result mutably through the post-hook chain.

`NarcHook::post_call` uses the new seam: when a tool result leaks a secret it
still raises a `Severity::Block` alert, but now also replaces the matched
credential with `[REDACTED:<pattern-name>]` in the result content before it
reaches the model. Clean results pass through untouched, and injection patterns
in results remain surveillance-only (detected and alerted, not rewritten).
