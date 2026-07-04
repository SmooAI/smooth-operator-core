---
'@smooai/smooth-operator-core': minor
---

SEP host: extension integrity verification + subprocess env hardening (th-210910).

SEP extensions are spawned as subprocesses (JSON-RPC over stdio). They were
previously launched with the host's full environment and ambient authority.
This lands the portable, high-value subset of hardening:

- **Integrity verification** — a second gate after the load allow-list. When a
  manifest pins `[run] sha256`, the host hashes the resolved command binary
  before spawning and refuses (both initial load and hot reload) on mismatch.
  When no pin is set, the observed hash is logged so a consumer can pin it
  (TOFU). Pinned-but-unresolvable commands are refused.
- **Environment scrub** — the child no longer inherits the host environment.
  The spawn does `.env_clear()` and passes through only a small allow-list of
  launch essentials (`PATH`, `HOME`, locale, `TMPDIR`, `TERM`, `SystemRoot`)
  plus the manifest's explicit `[run] env`. Ambient secrets (cloud creds, API
  tokens) can no longer leak into an extension via inherited env — the
  lethal-trifecta concern.

OS-specific sandboxing (Linux seccomp-bpf, uid/gid drop, Landlock; macOS
`sandbox_init`) is explicitly out of scope and tracked as the next increment.
