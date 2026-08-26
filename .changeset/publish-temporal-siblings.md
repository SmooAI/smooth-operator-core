---
'@smooai/smooth-operator-core': patch
---

fix(release): actually publish the TS/Python/.NET temporal packages (th-8a0b45)

The three non-Rust temporal siblings shipped in core #168/#169/#173 were never
published anywhere: `scripts/ci-publish.mjs` had rows only for the packages
that existed before them, so `@smooai/smooth-operator-temporal` (npm),
`smooai-smooth-operator-temporal` (PyPI) and `SmooAI.SmoothOperator.Temporal`
(NuGet) 404'd on every registry while each release run reported success — the
Rust temporal crate, which did have a row, published fine. They were also
missing from `scripts/sync-versions.mjs`, so their manifests drifted at their
birth versions (1.9.9 / 1.8.10 / 1.8.10) — meaning even a hand-run publish
would have shipped the wrong version.

Adds the three publish rows (ordered after their core siblings, since each
depends on the core package resolving on its registry), the four version
anchors (manifest + Python lockfile), and fixes the .NET temporal csproj's
`PackageReadmeFile` pointing at a README path that `dotnet pack` cannot find —
found because the orchestrator's DRY_RUN actually packs, which is the first
time anything ever packed this project for release.

Proven by `DRY_RUN=true node scripts/ci-publish.mjs`: all three siblings
pack/validate and report would-publish at the canonical version; this
changeset's own release run is what publishes them for real.
