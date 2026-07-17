---
'@smooai/smooth-operator-core': patch
---

Release infra: Changesets now drives lockstep publishing of every polyglot artifact (npm + crates.io + NuGet + PyPI + Go tag) from a single canonical version. Adds `scripts/sync-versions.mjs` (propagates the npm version to Rust/.NET/Python/Go manifests) and `scripts/ci-publish.mjs` (idempotent, skip-if-already-published, DRY_RUN) wired into `release.yml`. The per-language `publish-*.yml` workflows remain as manual fallbacks.
