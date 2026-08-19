---
'@smooai/smooth-operator-core': patch
---

Python: close a fail-open hole in `Clearance` and bound the human-in-the-loop gate.

**`Clearance` had no `"*"` wildcard, so a deny-all clearance failed OPEN.** Rust's
`Clearance::deny_all()` *is* `deny_tools = ["*"]`, and its `allows()` matches
`t == "*" || t == tool`. Python compared literally (`if tool in self.deny_tools`), so a
role definition shared with — or migrated from — Rust carrying `deny = ["*"]` permitted
**every** tool: `Clearance.deny("*").is_allowed("bash")` returned `True`. `is_allowed`
now denies when `"*"` is present, and `Clearance.is_deny_all()` exists for parity.
The allow side is deliberately unchanged: Rust matches allow entries literally, so
`"*"` is not a wildcard there either — a test now pins that so it can't drift open.

**The human gate had no timeout, where Rust fails closed.** `SmoothAgent` awaited
`gate.request_approval(...)` unbounded, and `PermissionHook.with_approver`'s timeout was
`float | None = None` with `None` skipping the wait entirely. Rust makes it a
non-optional `Duration` on both HITL surfaces and fails closed on elapse. An approval UI
whose socket drops therefore left the turn holding its connection, checkpoint lock and
concurrency slot indefinitely. Both surfaces are now always bounded, defaulting to
`DEFAULT_APPROVAL_TIMEOUT_SECONDS` (300s, the value Rust's `agent.rs` wires), tunable via
the new `AgentOptions.approval_timeout_seconds`. On elapse the tool never runs and the
model is told approval timed out. The Temporal HITL gates stay untimed in both engines —
that is deliberate parity, not a divergence.

Also in the same path: the approve-always grant write (`read_text` / `write_text` /
`os.replace` on `wonk-allow.toml`) now runs via `asyncio.to_thread` instead of blocking
the event loop for every concurrent turn inside `await hook.pre_call(...)`.
