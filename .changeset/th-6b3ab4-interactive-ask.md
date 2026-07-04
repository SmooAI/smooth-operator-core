---
'@smooai/smooth-operator-core': minor
---

th-6b3ab4: route an `Ask` permission verdict to a human instead of always failing closed.

The permission gate (th-d32ce6) blocked every `Ask` verdict, since the crate had
no interactive approver. `PermissionHook` now accepts an optional approver over
the same `HumanRequest`/`HumanResponse` bridge `ConfirmationHook` already uses
(`human_channel()`):

- **`PermissionHook::with_approver(tx, rx, timeout)`** — on an `Ask`, sends a
  `HumanRequest::Confirm` and blocks (up to `timeout`) on the response. Approve
  lets the call run; deny / timeout / dropped channel all block (fail-closed).
- **`Agent::with_extension_host`** wires the approver automatically when a human
  channel is present (via `Agent::with_human_channel`), with a 5-minute default
  window; with no channel the hook fails closed exactly as before.
- **A `Deny` is never routed to the human** — circuit-breakers (credential
  paths, `rm -rf /`, pipe-to-shell, env dumps, dangerous domains) stay
  non-waivable. Covered by a regression test asserting no prompt is sent.

Persisted allow-lists (smooth's `wonk-allow.toml`, "approve and don't ask
again") remain a follow-up — every `Ask` is currently approve-once.
