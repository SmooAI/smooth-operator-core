---
'@smooai/smooth-operator-core': minor
---

Fix a `Clearance` fail-open on a `"*"` deny entry in the TypeScript, Go and .NET engines.

Rust treats a deny entry of `"*"` as "deny everything" — `Clearance::allows` does
`self.deny_tools.iter().any(|t| t == "*" || t == tool)`, and `Clearance::deny_all()`
*is* `deny_tools = ["*"]`. The other three engines compared deny entries **literally**
against a set, so `"*"` matched no real tool name and the clearance **permitted every
tool**. A role definition authored for — or migrated from — Rust carrying
`deny = ["*"]` therefore granted full tool access in TypeScript, Go and .NET: the
strictest clearance the reference engine can express became the weakest.

These three engines also carry a separate `denyEverything` / `DenyEverything` boolean
that Rust has no equivalent of, and *that* path always worked. So the two ways of
spelling deny-all disagreed, and only the Rust-native spelling failed open. Each engine
now has an `isDenyAll()` / `IsDenyAll()` that honours **both** representations, and the
allow-check routes through it — a single guard, since every consumer of a clearance
(`agent.ts`, `agent.go`, `SubagentDispatcher.cs`) already funnels through that one
method.

The **allow** side is deliberately left literal. Rust has no wildcard branch there
either (`allow_tools.iter().any(|t| t == tool)`), so `allow("*")` permits only a tool
literally named `*` in all five engines. That is fail-closed and correct; each language
now pins it with a test so it cannot drift open later.

Verified per language by reverting *only* the semantic line — leaving the new
`IsDenyAll` API in place — and confirming the regression tests fail on the assertion
rather than erroring on a missing method. In that broken state Go reported
`DenyClearance("*").IsAllowed("bash") = true`, the fail-open observed directly. With the
fix: TypeScript 425 passed / 1 skipped, Go all four packages `ok` (`go vet` and `gofmt`
clean), .NET 409 passed / 2 skipped plus Temporal 5 passed / 4 skipped, Release build
with 0 warnings.

Completes the five-engine convergence started for Python in #182, which fixed the same
defect there and identified these three remaining sites.
