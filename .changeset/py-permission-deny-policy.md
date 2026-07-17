---
"@smooai/smooth-operator-core": minor
---

feat(python): permission engine + consumer deny policy (parity with the Rust reference)

Python port of the Rust engine's tool-call permission system and the new deny
policy (pearl th-ab0437; mirrors `permission.rs`, `permission_grants.rs`,
`deny_policy.rs`). Three new modules, all built on the existing `ToolHook` seam:

- **`permission`** — `AutoMode` (Ask / AcceptEdits / DenyUnmatched / Bypass, with
  `SMOOTH_AUTO_MODE` parsing), the `Verdict` union (Allow / Deny / Ask), and the
  pure `decide(mode, tool_name, args)` classifier faithfully reproducing every
  circuit-breaker: dangerous-CLI substrings, structural `curl … | sh` (across the
  pipe, sudo/wrapper-aware), credential/dotenv paths, process-env dumps
  (`env`/`printenv`/`$SECRET` expansions, command-substitution-proof), dangerous
  domains, `split_compound` / `strip_wrappers_and_sudo`, and the safe read-only
  bash/git allow-set. `PermissionHook` (a `ToolHook`) enforces it: `pre_call`
  raises on Deny; an Ask consults stored grants then routes to a `HumanGate`
  approver (fail-closed on timeout / no approver).
- **`permission_grants`** — the `wonk-allow.toml` allow-list (`PermissionGrants`,
  `NetworkGrant`/`ToolGrant`/`BashGrant`, `SharedGrants`, atomic
  `append_grant`, layered user+project load). A grant can only upgrade an Ask,
  never waive a Deny.
- **`deny_policy`** — `DenyPolicy` = declarative `DenyRules` (TOML: `[tools]` /
  `[bash]` / `[network]` / `[paths]` deny lists, same section style as grants) +
  a `DenyPredicate` ABC for semantic checks. Evaluated **first** in `pre_call`, so
  a policy match is a circuit-breaker no grant waives and no mode downgrades.

Wired into `AgentOptions` via `permission_mode` + `deny_policy` — when either is
set a `PermissionHook` is prepended so it gates every call first (a `deny_policy`
alone activates a Bypass-mode gate: built-in breakers + policy only). Purely
additive: with neither set, enforcement is byte-identical to before.
`HumanDecision` gains `APPROVED_ALWAYS` (persist a grant). Adversarial tests
ported from the Rust suites (sudo/compound/wrapper bash, network suffix+glob,
path R/W, predicate some/none, deny-beats-grant, survives-Bypass, TOML
round-trip).
