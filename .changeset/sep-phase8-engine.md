---
'@smooai/smooth-operator-core': minor
---

SEP Phase 8 (engine) — long-tail pi parity:

- **Inter-extension bus**: `bus/publish` now fans out as a `bus/event` observe
  event to every other extension subscribed to it (`BusRegistry` shares the
  loaded extensions' process + subscription handles; a `Weak` process ref avoids
  a reference cycle; a hot reload's subscription swap is reflected with no
  re-registration).
- **`context` hook wired**: extensions can replace the entire message array the
  LLM sees each iteration (pi's `context` middleware analog) via a pi-friendly
  `{role, content}` wire shape. Zero-copy and skipped when no extension declares
  the hook (`any_hook` gate; new optional `registrations.hooks` list).
- **`before_agent_start` hook wired**: extensions can rewrite the system prompt
  once at run start, composing with (never replacing) the resolved persona.
  Both hooks fire on the `run` and `run_with_channel` paths.
- **Render-block v2 keybinding routing**: `ExtensionHost::dispatch_widget_key`
  targets one extension's active widget with a `widget/key` notification,
  bypassing the observe subscription filter.
- **Declarative message renderers**: `registrations.message_renderers` (a custom
  message `tag` → render-block template) surfaced via
  `ExtensionHost::message_renderers()`; data-only, frontend renders.
