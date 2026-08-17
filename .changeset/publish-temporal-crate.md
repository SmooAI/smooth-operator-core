---
"@smooai/smooth-operator-core": patch
---

feat(rust): publish `smooai-smooth-operator-temporal` to crates.io

The Temporal durable-execution backend (ADR-030) was `publish = false`, which meant
nothing outside this repo could depend on it. That was the sole blocker on
`smooth-operator-server` selecting a durable backend: the server consumes the engine
from crates.io and is itself published, and cargo refuses to publish a crate that
declares a git or path dependency — even an optional one behind an off-by-default
feature, because packaging validates the manifest rather than the resolved feature
set. Brent's call was to publish rather than work around it, accepting that the
pinned preview SDK becomes publicly visible and publicly versioned.

What changed:

- `rust/smooth-operator-temporal/Cargo.toml` — dropped `publish = false`, added the
  metadata crates.io wants (`repository`, `readme`, `keywords`, `categories`), and
  gave the core dependency a version requirement alongside its path. That
  requirement is `^1.8` rather than the exact lockstep version so it stays
  satisfiable across patch releases without needing to be re-synced.
- Brought the crate into the version lockstep. It was drifting at `0.14.0` while
  every other artifact was on the `1.8.x` anchor, which was harmless while its
  version was inert but would now make `ci-publish`'s `cratesHasVersion` check never
  match — it would try to publish the stale version forever. `sync-versions.mjs`
  gained an anchor for it, so it moves with the anchor like the other five manifests.
  Its first published version is therefore `1.8.x`; since it has never been
  published, no public version history is disturbed.
- `ci-publish.mjs` publishes it, ordered **after** the core crate — cargo will not
  publish a crate whose dependency is not yet on the index.
- `pr-checks.yml` gained its own `cargo publish --dry-run` for it. Packaging rules
  are per-manifest, so the core crate's existing dry-run cannot see a bad manifest
  here; without this, a stray path dep or missing metadata would surface only at
  release and take the whole lockstep publish down with it. That is exactly how the
  original blocker stayed hidden.

A default build still compiles no `temporalio-*` at all — the SDK stays behind the
off-by-default `temporal` feature, so consumers who do not opt in remain zero-infra.
Verified: the crate packages and its verify build compiles against the **published**
core from crates.io (not the local path), and under `--features temporal` clippy is
clean and all four e2e pass against a real ephemeral Temporal server (1.31.2),
including the durable HITL signal gate and the durable timer.
