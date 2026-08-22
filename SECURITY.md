# Security Policy

smooth-operator-core is the engine that runs AI agents: it drives models, executes tools,
checkpoints state, holds memory, and enforces the deny-policy and permission gates that decide what
an agent is structurally allowed to do. A bug here can reach data and systems well outside this
repository. Please report suspected vulnerabilities privately.

## Reporting a vulnerability

**Email [security@smoo.ai](mailto:security@smoo.ai).**

Please do **not** open a public issue, a Discussion, or a pull request for a suspected
vulnerability. A public report is a public exploit for everyone running the affected version.

<!--
TODO(maintainers): GitHub private vulnerability reporting is DISABLED on this repo
(checked 2026-08-22: `gh api repos/SmooAI/smooth-operator-core/private-vulnerability-reporting`
returned {"enabled":false}). Enable it under Settings → Security → "Private vulnerability
reporting", then list https://github.com/SmooAI/smooth-operator-core/security/advisories/new above
as the preferred channel — it gives reporters a tracked thread and a CVE path that email does not.
-->

Include whatever you have; a partial report is better than a silent one:

- The **version** you are on, and **which language** — the Rust reference or the TypeScript, Python,
  Go, or .NET port. The same logical bug is often present in one and not the others, and the deny
  policy in particular is furthest hardened in Rust.
- How you are using the engine — embedded as a library, with the Temporal durable-execution backend,
  or through smooth-operator / the hosted Smoo AI platform.
- Reproduction steps, a proof of concept, and the impact you believe it has.
- Whether any of it is already public.

If you would like to be credited in the advisory, say so and give us the name or handle to use.

## What to expect

smooth-operator-core is maintained by a small team at Smoo AI. We will acknowledge your report, tell
you whether we could reproduce it, and keep you updated while we work on a fix. We are not going to
publish an hours-and-minutes SLA we cannot staff around the clock — but silence is a failure of this
process, not an answer. If a report goes unanswered, reply on the same thread and escalate.

We ask that you give us a reasonable window to ship a fix before disclosing publicly, and we will
credit you in the advisory unless you would rather stay anonymous.

<!--
TODO(maintainers): if someone takes ownership of the security@smoo.ai rota and can commit to a
concrete acknowledgement window (e.g. "within N business days"), replace the paragraph above with
that number. It was left unquantified deliberately rather than promising a response time nobody
had agreed to.
-->

## Supported versions

Only the **latest published release** receives security fixes.

Every published artifact — npm, NuGet, PyPI, crates.io, and the Go module — ships at one lockstep
version, so "latest" is the same number in every language. There are no maintained release
branches; fixes land on `main` and go out in the next release.

## Scope

In scope: the engine in every language (`rust/`, `typescript/`, `python/`, `go/`, `dotnet/`), the
Temporal durable-execution backend, and the schemas in `spec/`.

Worth calling out for an agent engine specifically: **deny-policy and permission-gate bypasses** —
anything that lets a tool call proceed which the policy should have refused — prompt-injection paths
that cross a trust boundary (untrusted content reaching a tool with real authority), tool-sandbox
escapes, checkpoint or memory poisoning, and anything that leaks one tenant's state into another
agent run.

Out of scope: findings that require an already-compromised host or an already-leaked credential,
missing hardening with no demonstrated impact, and results from automated scanners submitted without
a working proof of concept. Vulnerabilities in the hosted Smoo AI platform (rather than this code)
also go to security@smoo.ai — same address, we will route it.
