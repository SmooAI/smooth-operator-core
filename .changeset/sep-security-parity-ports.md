---
"@smooai/smooth-operator-core": patch
---

fix(go,ts,python,dotnet): SEP security parity — scrub the extension subprocess env, and guard cross-tool `tool_call` Modify

Two security behaviors the Rust reference host has had since `th-210910` /
`th-f0e020` were missing from all four ports, so a repo whose pitch is
"security-first design" shipped four engines that were not.

**Env scrubbing on extension spawn.** Go, TypeScript, Python and .NET all handed
an extension subprocess the host's FULL ambient environment (`os.Environ()`,
`{...process.env}`, `{**os.environ}`, and .NET's pre-populated
`ProcessStartInfo.Environment`). Any third-party extension therefore inherited
`AWS_SECRET_ACCESS_KEY`, `GITHUB_TOKEN` and every other ambient credential —
the lethal-trifecta concern. Each port now mirrors Rust's `ENV_PASSTHROUGH`
allow-list (`PATH`, `HOME`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TMPDIR`, `TERM`,
`SystemRoot` — launch essentials, none secret) via a pure, injectable
`build_child_env` and clears everything else. The manifest's `[run] env` is
overlaid on top, so an extension can still *set* — but never silently
*inherit* — a var.

**Cross-tool `tool_call` Modify guard.** The `tool_call` hook fires over EVERY
pending call the model made, native `bash`/`file-write` included, and the ports
applied a `Modify` verdict verbatim as a full `{tool, arguments}` replacement.
Enabling any extension therefore let its hook rewrite a pending native `bash`
call's arguments, or redirect a call to a different tool, with zero oversight.
Each port now runs Rust's `guard_tool_call_modify`: a `Modify` is honored only
when it does not change which tool runs AND the acting extension owns the tool
(`<ext>.<tool>`); otherwise it is downgraded to `Continue`, preserving the
ORIGINAL call, and logged. `Continue`/`Block` are untouched — an extension
blocking any call stays safe and useful; only MUTATION is scoped.

Rust's adversarial tests are ported to each language (native-tool rewrite,
foreign-extension rewrite, tool redirect, dot-boundary ownership, ambient-secret
scrub, manifest-env precedence).
