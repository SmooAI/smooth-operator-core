---
'@smooai/smooth-operator-core': patch
---

Build the package before packing so the published tarball actually contains
`dist/`. The release ran `changeset publish` with no build step and the package
had no `prepack`/`prepare` hook, so recent versions (e.g. 0.9.0) shipped without
compiled output — every `@smooai/smooth-operator-core` import 404s. Add
`"prepack": "pnpm run build"` so `npm publish` builds `dist/` at pack time.
