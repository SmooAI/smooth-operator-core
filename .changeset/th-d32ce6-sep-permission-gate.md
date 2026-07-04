---
'@smooai/smooth-operator-core': minor
---

th-d32ce6: gate SEP extension (and native) tool calls behind a permission classifier.

The Smooth Extension Protocol host executed extension-contributed tools with no
permission gate — once an extension cleared the load allowlist it ran any tool
freely: no allow/ask/deny model, no dangerous-command classifier, no
circuit-breakers.

New `permission` module (`src/permission.rs`) ports the classification model
natively from smooth's `smooth-bigsmooth::auto_mode` (it can't be imported —
smooth depends on this crate):

- **`decide(mode, tool_name, args) -> Verdict`** — pure, deterministic
  classifier. Read-only → Allow, mutating → Ask, dangerous → Deny.
- **Hard circuit-breakers (deny in every mode, incl. `Bypass`)**: credential
  paths (`~/.ssh/id_*`, `~/.aws/credentials`, dotenv files, smooth's own secret
  stores), `rm -rf /` family, `curl … | sh` / pipe-to-shell (incl. `sudo bash`
  sinks), fork bombs, `mkfs`/`dd`, env-dumps (`env`/`printenv`/`$SECRET`
  echoes, `$(env)` substitution smuggling), and dangerous domains
  (pastebin/transfer.sh/ngrok/crypto). Compound commands (`ls && rm -rf /`) are
  split so a safe first command can't shield a dangerous tail.
- **Modes via `SMOOTH_AUTO_MODE`**: `ask` (default) / `accept-edits` / `deny`
  (headless) / `bypass`.
- **`PermissionHook`** (`ToolHook::pre_call`) blocks on Deny and — fail-closed,
  since this crate has no interactive approver — on Ask.

Wired onto the agent's `ToolRegistry` in `Agent::with_extension_host`, gating
every tool call. New `Agent::with_permission_mode(mode)` lets a consumer set the
posture (before attaching the host) without the `SMOOTH_AUTO_MODE` env var.

Secure by default: unmatched extension tools now require approval and, with no
approver, are blocked. Consumers that trust their extensions opt into
`AutoMode::Bypass` (hard circuit-breakers still fire).

Interactive Ask routing (a confirm bridge so Ask can prompt a human instead of
failing closed) is deferred to a follow-up pearl.
