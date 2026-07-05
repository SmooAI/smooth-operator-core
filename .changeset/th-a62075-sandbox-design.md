---
'@smooai/smooth-operator-core': minor
---

th-a62075: microVM isolation for SEP extensions (design + first increment).

Closes the one structural gap the tool-layer guardrails cannot see: a
compromised extension *binary* issuing syscalls directly against the host
kernel (process tampering, mount/bpf/kernel manipulation, credential-file
reads) never crosses the SEP/tool channel, so the permission gate + Narc never
observe it.

**Approach: microsandbox microVM per extension** (`docs/Extension-Sandboxing-Design.md`).
A seccomp/Landlock in-process tier was designed then dropped as
over-engineering — microsandbox is stronger (separate guest kernel),
cross-platform (macOS + Linux, unlike Linux-only seccomp), covers network
egress natively, and is already on the fleet. It is driven by the `msb` CLI
(runtime shell-out, like `smooth-dolt`), so operator-core gains **no cargo
dependency**.

This increment:
- Manifest `[sandbox]` section (`image`, `memory`, `cpus`, `network` =
  `none`/`egress`, `allow_domains`) → `SandboxSpec` on `ExtensionManifest` and
  `SpawnSpec`.
- Pure, exhaustively-tested `build_msb_command` argv builder (the isolation
  surface): `--no-net` by default, default-deny + per-domain `--net-rule allow@`
  for egress, empty-egress fails safe to no-net, scrubbed env forwarded as
  sorted `-e` pairs, image + attached-mode guest command.
- `SMOOTH_EXTENSION_SANDBOX` gate (default **off** → direct host spawn,
  unchanged and non-breaking). When on + a `[sandbox]` image is present, the
  extension runs in a microVM; if `msb` is absent it **fails closed** rather
  than run untrusted code unisolated.
- Extensions ship their code in the image (no writable host bind-mount — `msb`
  0.4.6 `-v` has no read-only mode); the image is the integrity anchor, so the
  host-binary `sha256` pin is skipped on the sandboxed path.

Follow-ups: `--snapshot` pre-warming (th-4b4544), Events API (th-dd84b5),
a smooth-provided base image.
