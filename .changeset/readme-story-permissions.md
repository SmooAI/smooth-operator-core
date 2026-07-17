---
"@smooai/smooth-operator-core": patch
---

docs: rewrite the root + per-language package READMEs as registry landing pages that tell a story

Every README (root and the Rust / TypeScript / Python / Go / .NET package pages)
now opens with a hook and a narrative arc — problem → one engine in five
languages → observe→think→act → the permission gate + deny-policy that makes an
agent safe to point at production → build → get started. Each package page leads
with a tight agent-plus-tool quickstart in its own idiom (the mock scripted to
call the tool, then answer) and a permissions/deny-policy example using that
language's real API (`with_deny_policy` in Rust, `denyPolicy`/`permissionMode`
options in TS/Py, `WithDenyPolicy` in Go/C#).

Adds the headline permission system + deny-policy (AutoMode ask / accept-edits /
deny-unmatched / bypass, circuit-breakers, declarative TOML rules + semantic
predicates) to the feature surface, refreshes the polyglot table
(language → package → registry), and fixes stale test-count claims. Docs only —
no code changes.
