---
'@smooai/smooth-operator-core-monorepo': minor
---

Migrate the TypeScript engine (`@smooai/smooth-operator-core`, v0.1.0) into `smooth-operator-core`, where it is now published from.

The engine previously lived in `smooth-operator/typescript/core`; it belongs here in the polyglot engine repo alongside the Rust reference, the C# core, and the Python core. This is an additive move — the package name and version are preserved exactly for registry continuity, and the engine is fully self-contained (depends only on `openai`; no workspace deps on any other `smooth-operator` package).

- Wired the repo as a pnpm workspace (`pnpm-workspace.yaml` → `typescript/*`).
- Added a `TypeScript` PR/push workflow (`ts-checks.yml`): typecheck + test.
- Added a release-gated `Publish npm` workflow (`publish-npm.yml`): dry-run-first manual dispatch + `npm-v*` tag trigger; never publishes from a branch push.
