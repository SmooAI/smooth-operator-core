---
'@smooai/smooth-operator-core': patch
---

th-64b1ee: audit + harden the `tool_search` meta-tool against prompt-injection tool promotion.

Verified the critical defense claim: `PermissionHook` (a `ToolHook`) gates the
*invocation* of a promoted-but-forbidden tool. `ToolRegistry::execute` runs all
pre-hooks before resolving the tool, and `tool_by_name` resolves promoted-deferred
tools on the same path as eager ones — so a prompt-injection payload that makes a
read-only agent `tool_search` a deferred `bash` exec tool cannot bypass the gate:
the dangerous invocation is still denied. Added a regression test
(`permission_hook_gates_promoted_deferred_tool`) that promotes a deferred `bash`
via `tool_search`, then asserts a dangerous command is blocked (body never runs,
execution counter stays 0) while safe calls still run.

Also: `tool_search` now emits a `tracing::info!(target: "tool_search")` audit line
for every promotion (query + promoted tool names) and returns the promoted names in
its JSON payload (`promoted` field) so the privilege change is observable, not just
a side-effecting log. Substring matching left as-is — the `MAX_MATCHES` cap plus the
`PermissionHook` invocation gate are sufficient; no per-tool promote allowlist added.
