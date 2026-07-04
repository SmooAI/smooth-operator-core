---
'@smooai/smooth-operator-core': minor
---

th-22bfc1: Persist human approvals so the SEP permission gate stops being approve-once.

Ports smooth's `wonk-allow.toml` allow-list into the engine (`permission_grants`
module). The `PermissionHook` now consults the allow-list **before** prompting on
an `Ask` verdict: a matching stored grant auto-approves silently, and answering
`HumanResponse::ApprovedAlways` (a new additive variant) persists a grant so the
next identical `Ask` never prompts again.

- Two stacked TOML files, `~/.smooth/wonk-allow.toml` (user) and
  `<cwd>/.smooth/wonk-allow.toml` (project, wins on collision), format
  compatible in spirit with smooth's.
- Grant kinds: `network` hosts (exact / `*.suffix` glob), `tools` (exact name),
  `bash` command prefixes (`"npm "`).
- A grant can only upgrade an `Ask` — it can **never** waive a `Deny`
  circuit-breaker (`rm -rf /`, credential paths, dangerous domains, …).
- Robust I/O: missing file → empty store, malformed file → surfaced error,
  atomic tempfile-then-rename writes.
