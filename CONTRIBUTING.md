# Contributing to smooth-operator-core

Thanks for being here. This file is the short version of how this repo actually works — the parts
that will get a PR sent back if you skip them. It is deliberately specific to
smooth-operator-core rather than generic advice.

**Where things go** — the last one is the rule people get wrong, and getting it wrong publishes a zero-day:

- **Bug or concrete task** → [an Issue](https://github.com/SmooAI/smooth-operator-core/issues)
- **Question, idea, or "how do I…"** → [a Discussion](https://github.com/SmooAI/smooth-operator-core/discussions), where it stays searchable for the next person who asks
- **Security vulnerability** → the private channel in [SECURITY.md](./SECURITY.md) ([security@smoo.ai](mailto:security@smoo.ai)), **never** either of the public two

## The four things that matter

### 1. Add a changeset, or your work is merged but never published

This is the one that bites hardest, because nothing fails loudly: your PR goes green, it merges, the
release workflow succeeds, and your change is simply not in any published artifact.

Every artifact here — npm, crates.io, NuGet, PyPI, the Go module — ships at **one shared version**,
and that version lives in exactly one package: **`@smooai/smooth-operator-core`**
(`typescript/core/package.json`). Changesets natively versions only npm packages, so
`scripts/sync-versions.mjs` stamps that number onto every other manifest — `rust/*/Cargo.toml`,
`dotnet/core/src/…csproj`, `python/core/pyproject.toml`, and `go/version.go` (which is the anchor the
publish scripts read for the `go/vX.Y.Z` tag). The consequence people miss: **a changeset that does
not name `@smooai/smooth-operator-core` cannot republish any non-npm artifact.** One changeset naming
that package covers all five languages.

```bash
pnpm changeset
```

Then, in the generated file, make sure the frontmatter names it:

```md
---
'@smooai/smooth-operator-core': minor
---

feat(rust): what changed and why it matters
```

> **Note:** the sibling repo [smooth-operator](https://github.com/SmooAI/smooth-operator) enforces
> this with an **Anchor Guard** CI check, because it has a confusable near-neighbour package name
> that silently swallowed two .NET releases. This repo has **no such check** — nothing here will
> tell you the changeset named the wrong thing, so it is on the author and the reviewer. If you are
> reviewing, read the changeset frontmatter.

Write the changeset body like a changelog entry someone will read six months from now: what changed,
why, and what it means for a consumer. Look at the existing files in `.changeset/` for the register.

### 2. Rust is the reference implementation; the ports mirror it

`rust/smooth-operator-core` is the source of truth for behaviour. The TypeScript, Python, Go, and
.NET engines are **native ports at parity** — the same engine, idiomatic in each language, held to a
shared eval suite. Parity is enforced by tests, not by mirroring type shapes.

So a behaviour change starts in Rust:

1. Change the Rust behaviour and its test.
2. If the **wire** changes, change the JSON Schemas in `spec/` too (see §3).
3. Port the behaviour to the other languages, each with its own parity test named/scoped to its Rust
   counterpart so a gap stays visible.

A port-only PR (bringing one language up to a behaviour Rust already has) is very welcome and does
not need the Rust step — that behaviour already exists. What we want to avoid is a behaviour that
exists *only* in a port, because then there is no reference to check the others against.

[`docs/Polyglot-Engines.md`](./docs/Polyglot-Engines.md) has the honest parity picture per language,
including where the ports are deliberately behind (extension sandboxing is furthest along in Rust).

### 3. `spec/` is the source of truth, and generated types are committed

`spec/` holds the language-neutral JSON Schemas — `envelope.schema.json`, `actions/`, `events/`,
`domain/`, plus `evals/`, `providers/`, and `narc/` — with canonical instances in
`spec/conformance/`. Every language generates its protocol types from those schemas rather than
declaring them by hand, which is what keeps the languages from drifting.

The generated output is **committed** (in this repo, `go/protocol/types_gen.go`). Committed means
reviewable — but it also means it is possible to hand-edit, and a hand-edit is drift that no schema
check will catch. **Change the schema and regenerate**; never patch a generated file directly.
Per-language generator configuration is documented in
[`spec/codegen/README.md`](./spec/codegen/README.md).

### 4. Tests: run them, and prove the new one fails first

A new test should be **shown to fail without its fix**. Write it, watch it go red, then implement
until it is green. A test that passes before the change is not testing the change — it is decoration
that will keep passing when the behaviour regresses.

Per language, matching what CI runs:

```bash
# Rust (the reference)
cd rust
cargo fmt --all -- --check
cargo clippy --workspace --all-targets -- -D warnings
cargo test --workspace
# the Temporal backend is behind a feature flag and has its own CI rows:
cargo clippy -p smooai-smooth-operator-temporal --features temporal --all-targets -- -D warnings
cargo test -p smooai-smooth-operator-temporal --features temporal

# TypeScript
pnpm install --frozen-lockfile
pnpm --filter @smooai/smooth-operator-core typecheck
pnpm --filter @smooai/smooth-operator-core test

# Python
cd python/core && uv sync --locked
uv run ruff format --check src tests && uv run ruff check src tests
uv run pytest tests/ -q

# Go
cd go && gofmt -l . && go vet ./... && go test -race ./...

# .NET
dotnet test dotnet/SmooAI.SmoothOperator.Core.slnx -c Release --filter "FullyQualifiedName!~LiveE2E"
```

Note the two things CI does that are easy to miss locally: `cargo clippy` runs with
`--all-targets`, so it lints your **tests** too — `cargo clippy -p <crate>` alone will not reproduce
a red CI row. And PR checks include `cargo publish --dry-run … --locked`, which catches a stray
path or git dependency that builds fine but cannot be published.

Eval suites (the LLM-judged scenarios, in every language) are gated on `SMOOTH_AGENT_E2E=1` plus
`SMOOAI_GATEWAY_KEY` and **skip cleanly** without them — they run nightly, not on your PR. A test
that fails for a missing credential trains everyone to ignore red CI, so keep new live tests gated
the same way.

## Pull requests

- Branch off `main`, keep the PR focused on one thing.
- Say what changed and **why**. The why is the part reviewers cannot reconstruct.
- Include the changeset (§1) unless the PR is genuinely docs- or test-only — and check that its
  frontmatter names `@smooai/smooth-operator-core`, since nothing here checks it for you.
- Get CI green. If a check is red for a reason you believe is unrelated, say so in the PR rather
  than merging past it.
- New behaviour needs a test that failed before your fix (§4).

## Code of conduct

By participating you agree to the [Code of Conduct](./CODE_OF_CONDUCT.md).

## Licence

Contributions are accepted under the [MIT Licence](./LICENSE), same as the project.
