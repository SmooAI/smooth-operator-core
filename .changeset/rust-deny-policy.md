---
"@smooai/smooth-operator-core": minor
---

feat(rust): consumer-supplied deny policy for the permission engine (reference impl)

Adds a new `deny_policy` module to the Rust engine — a consumer-declarable deny
tier that the hardcoded circuit-breakers and allow-only grants could not express
("never the prod AWS profile", "deny the DB writer endpoint, reads go to the
replica", "no writes under `/prod`").

Two tiers, both circuit-breaker strength:

- **Declarative** `DenyRules` (serde/TOML, mirroring `permission_grants`'
  section style): `[tools] deny` (name globs), `[bash] deny_patterns` (compound-
  and sudo/wrapper-aware command prefixes/globs), `[network] deny_hosts` (suffix
  + `*.`/mid-string globs, reusing `domain_matches_suffix_list`), `[paths] deny`
  (path globs for Write/Read tools).
- **Predicate** `DenyPredicate` trait — boxed consumer checks for semantic cases
  the engine can't parse from strings (is this the prod account? the writer
  endpoint?).

Assembled into `DenyPolicy { declarative, predicates }` (`from_toml` + a builder
for predicates). Wired via `PermissionHook::with_deny_policy(...)` and
`Agent::with_deny_policy(...)`; evaluated **first** in `pre_call`, so a policy
match is a terminal deny that no stored grant can waive and that
`Bypass`/`AcceptEdits` cannot downgrade — the same tier as the built-in
breakers.

Purely additive: with no policy set, enforcement is byte-identical to before
(proven by test). This is the reference implementation the C#/TS/Python/Go ports
will mirror.
