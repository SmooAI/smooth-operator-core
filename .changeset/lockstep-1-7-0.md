---
"@smooai/smooth-operator-core": minor
---

First lockstep polyglot release. Changesets now drives publishing for every
language artifact (npm + crates.io + NuGet + PyPI + Go tag) at a single shared
version via `scripts/ci-publish.mjs`, with `scripts/sync-versions.mjs`
propagating the Changeset version to all manifests. This aligns the previously
divergent per-language versions (npm 0.22, Rust 0.16, .NET 1.6, Python 1.3) onto
one lockstep line at 1.7.0 — no registry downgrades.
