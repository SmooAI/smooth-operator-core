---
'@smooai/smooth-operator-core': minor
---

Port the permission system + deny-policy to the TypeScript engine, to parity with the Rust reference (pearl th-ab0437).

Adds a native tool-call permission gate mirroring `rust/smooth-operator-core`:

- **`AutoMode`** (`Ask` / `AcceptEdits` / `DenyUnmatched` / `Bypass`, plus `autoModeFromEnv`/`autoModeFromValue` reading `SMOOTH_AUTO_MODE`) and **`Verdict`** (an `allow`/`deny`/`ask` discriminated union).
- **`decide(mode, toolName, args)`** — the pure, deterministic classifier with all circuit-breakers faithfully reproduced (dangerous-CLI substrings, `curl | sh` pipe-to-shell, credential/dotenv path guard, env-dump guard, dangerous domains, compound-command splitting, `sudo`/wrapper stripping, safe read-only bash allow-list). Denies survive every mode, including `Bypass`.
- **`PermissionGrants`** — the allow-only grant store (`network`/`tools`/`bash` sections, TOML round-trip) that can upgrade an `Ask`, never waive a `Deny`.
- **`DenyPolicy`** — consumer-supplied declarative deny rules (`[tools]`/`[bash]`/`[network]`/`[paths]`, TOML) plus a `DenyPredicate` callback for semantic checks. Evaluated FIRST as a circuit-breaker tier: no grant waives it and no mode downgrades it.
- **`PermissionHook`** (implements the new `ToolHook` interface) wiring it together, with `Ask` routed to the existing `HumanGate` (new `approveAlways()` / `remember` for persistent grants) and failing closed when no approver is wired.

Wired into `SmoothAgent` via new options `permissionMode`, `denyPolicy`, and `permissionGrants`. Purely additive: with none set the gate is off and behaviour is unchanged.
