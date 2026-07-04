---
'@smooai/smooth-operator-core': minor
---

th-5f7227: scan SEP extension tool arguments + results for secrets and prompt injection.

The Smooth Extension Protocol host sent extension tool **arguments** to the
subprocess unscanned and returned the subprocess's tool **result** content to
the model verbatim — no secret-detection or prompt-injection scanning at the
extension boundary. The just-merged `PermissionHook` (th-d32ce6) gates
allow/ask/deny and the dangerous-command circuit-breakers, but does no content
scanning.

New `narc` module (`src/narc.rs`) ports smooth's `smooth-narc` surveillance
model natively (it can't be imported — smooth depends on this crate):

- **`NarcHook`** — a `ToolHook` installed on the extension-host `ToolRegistry`
  in `Agent::with_extension_host`, **after** the `PermissionHook` (permission
  gate first, then Narc scans the calls that clear it). Gated behind
  extension-host attachment, so non-extension agents are unaffected.
- **Secret detection** — 10 patterns (AWS access/secret keys, Anthropic/OpenAI
  keys, GitHub tokens, private keys, generic secrets, bearer tokens, base64
  keys, Stripe keys). Matches are redacted before logging.
- **Prompt-injection detection** — 8 patterns (instruction override, role
  hijack, system-prompt injection, jailbreak, base64 smuggling, data/URL
  exfiltration, smell URLs), each carrying a severity.
- **`pre_call`** blocks the call (`Err`) on a `Block`-severity injection pattern
  in the arguments (active data/URL exfiltration); lower-severity injection and
  any secret in the arguments are alerted (detect + log), not blocked — a tool
  arg legitimately carrying a secret is common enough that a hard block would be
  a footgun.
- **`post_call`** detects secrets/injection in the result and records + logs a
  severity alert, but **cannot redact** — the `post_call` seam takes an
  immutable `&ToolResult` and its `Err` is only logged by the registry.
  Redacting a leaked result requires a mutable seam change, deliberately out of
  scope here and filed as a follow-up.

Deliberately does **not** re-port smooth-narc's `CliGuard`/`WriteGuard` — the
`PermissionHook` already owns dangerous-command and write gating. Exhaustively
tested (30 tests): each secret pattern positive + near-miss negative, each
injection pattern, `pre_call` blocks on exfiltration, `post_call` detects a
secret leak in a result, and an integration test proving the hook is live on a
real `ToolRegistry`.
