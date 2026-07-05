# OS-Level Extension Sandboxing — Design

> Status: **Proposed** (design pass for pearl th-a62075, the OS-specific
> follow-up to th-210910). Implementation not yet started — this document
> is the decision this repo needs before writing platform code.

## Context

SEP extensions run as **host subprocesses** speaking JSON-RPC over stdio
(`ExtensionProcess::start_connection` in
`rust/smooth-operator-core/src/extension/process.rs`). They contribute tools
(`<ext>.<tool>`), event middleware, and `ui/*` dialogs to the agent.

Today an extension is launched with several layers of defense already in place:

1. **Load allowlist** — `SMOOTH_EXTENSIONS_ALLOW` is default-deny; only
   named extensions load at all.
2. **Integrity pin** (th-210910) — the binary's SHA-256 is verified against a
   manifest `[run] sha256` before spawn, on initial load *and* hot reload.
3. **Environment scrub** (th-210910) — `.env_clear()` + a six-var launch
   allowlist (`PATH`, `HOME`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TMPDIR`, `TERM`,
   `SystemRoot`) plus the manifest's explicit `env`. Ambient host secrets
   (cloud creds, tokens) never inherit.
4. **Only stdio fds** — Rust sets `CLOEXEC` on every other host fd; the child
   gets exactly its three JSON-RPC pipes.
5. **Per-tool permission gate** (th-d32ce6 / th-6b3ab4) — every tool call the
   extension makes is classified allow/ask/deny by `PermissionHook`, with hard
   circuit-breakers and interactive approval.
6. **Narc surveillance + redaction** (th-5f7227 / th-10eb50) — tool args and
   results are scanned for secrets/injection; secrets are redacted out of
   results.

**What is still missing:** all of the above constrains what the extension does
*through the SEP/tool channel*. None of it constrains what the extension
process does **directly against the host kernel** — a compromised or malicious
extension binary (one that passed the integrity pin because it was malicious
*before* it was pinned, or was compromised at its source) can still issue
arbitrary syscalls: read files outside the workspace, `ptrace` a sibling
process, `mount`, `bpf`, load a kernel module, open raw sockets, spawn its own
children that escape all the tool-layer policy. The tool-layer gate never sees
those — they don't go through SEP.

This document designs the OS-level containment layer that closes that gap.

## Non-goals (explicit boundaries)

- **Network policy is NOT an OS-sandbox concern here.** Whether an extension's
  *tool* may reach a host is already decided per-call by `PermissionHook`
  (network → ask; dangerous domains → deny). An OS-level network jail would
  either duplicate that (and get out of sync) or break legitimate extensions
  whose whole purpose is a network integration (e.g. `smooai-crm` calling
  `api.smoo.ai`). We deliberately leave egress control at the tool layer. The
  OS sandbox constrains the *process*, not its business logic.
- **Not a replacement for any existing layer.** This is defense-in-depth
  *underneath* integrity + env-scrub + the permission gate. If those are the
  lock on the door, this is the wall around the room.
- **Not full container/VM isolation.** The microVM path was removed (smooth
  pearl th-f4a801); we are hardening the in-process subprocess model, not
  bringing back a VM per extension.

## Threat model

The adversary is the **extension binary itself** after it starts running:
a supply-chain-compromised dependency, a backdoored build, or a bug an
attacker drives via crafted tool input. It already has: its own code
execution, its three stdio pipes, a scrubbed env, and read access to whatever
the host uid can read. We want to bound the *damage that execution can do to
the host*, without breaking a legitimate Node/Python/binary extension.

Concretely we want to deny, at the kernel boundary:

- Tampering with other processes: `ptrace`, `process_vm_readv/writev`,
  `kcmp`, `pidfd_*` cross-process.
- Privilege / kernel manipulation: `mount`, `umount2`, `pivot_root`,
  `setns`, `unshare`, `init_module`/`finit_module`, `delete_module`,
  `kexec_load`, `reboot`, `swapon/swapoff`, `bpf`, `keyctl`,
  `add_key`/`request_key`, `perf_event_open`, `ioperm`/`iopl`.
- Privilege escalation via the binary itself: gaining new privileges through
  setuid helpers.
- Filesystem reach outside the extension's working set: reading `~/.ssh`,
  `~/.aws`, other users' homes, `/etc/shadow`; writing anywhere but its own
  scratch + the workspace.

## Options considered

### Syscall filtering: allowlist vs denylist

| | Allowlist (deny by default) | Denylist (allow by default) |
|---|---|---|
| Strength | Maximal — unknown syscalls blocked | Bounded — only named dangers blocked |
| Breakage risk | **High** — Node/Python touch hundreds of syscalls (`futex`, `mmap`, `openat`, `epoll_*`, `io_uring`, `clone`, `statx`…); a missed one crashes the interpreter, and the set drifts across libc/runtime versions | **Low** — interpreters keep working; we only block syscalls no legitimate extension needs |
| Maintenance | Ongoing per-runtime tuning | Stable — the dangerous set rarely changes |
| Philosophy fit | — | Matches the project's allow/ask/**deny** auto-mode model: deny only the unambiguously dangerous, keep function intact ("non-destructive auto mode") |

**Decision: denylist.** A seccomp-bpf filter that returns `EPERM`/`SIGSYS`
for the dangerous set above and allows everything else. This is the same
posture as the tool-layer gate: we are precise about what is forbidden and
permissive about the rest, so a normal extension never notices the sandbox.
An allowlist is stronger on paper but its failure mode is "correct extension
mysteriously crashes," which erodes trust in the whole guardrail system and
invites people to disable it — a weaker equilibrium than a denylist that is
always on.

### Filesystem restriction (Linux): Landlock vs namespaces vs none

- **Landlock** (kernel 5.13+, unprivileged, `landlock` crate): grant the
  child read on { extension dir, workspace root, system lib/bin paths needed
  to exec the interpreter, `TMPDIR` } and write on { `TMPDIR`, workspace
  scratch }. No new privileges required, composes with seccomp.
- **Mount namespaces**: stronger but need `CLONE_NEWUSER`/`CLONE_NEWNS`
  plumbing, break on some hardened kernels, and complicate PATH/interpreter
  resolution.
- **None**: rely only on syscall filter — leaves credential-file reads open
  (the exact lethal-trifecta risk the tool layer already treats as a
  circuit-breaker).

**Decision: Landlock where available, degrade to seccomp-only below 5.13.**
FS reach is the highest-value restriction after syscall danger — it directly
blocks the "read `~/.aws/credentials` and exfil" path at the process level,
backstopping the tool-layer credential-path breaker.

### Privilege drop (uid/gid + no_new_privs)

- `no_new_privs` (`PR_SET_NO_NEW_PRIVS`): always set — cheap, required for an
  unprivileged seccomp filter anyway, and blocks setuid escalation.
- uid/gid drop: only meaningful when the host runs privileged. The common case
  (developer laptop, `th` running as the user) has nothing to drop to safely.
  **Decision:** set `no_new_privs` unconditionally; perform a uid/gid drop
  only when the host is root **and** a drop target is configured — otherwise
  skip (documented), never silently run as a wrong uid.

### macOS

No Landlock/seccomp. Options: `sandbox_init` with an SBPL profile (the
`sandbox-exec` mechanism — officially deprecated but still functional and
widely used), or App Sandbox (requires code-signing entitlements — not viable
for arbitrary spawned binaries). **Decision:** a `sandbox_init` deny-by-default
profile allowing stdio, read of the extension bundle + system frameworks +
`TMPDIR`, and write of `TMPDIR`/workspace scratch. Network left to the tool
layer as above. Treated as best-effort parity; if the API is unavailable,
fall back to the failure policy below.

### Windows

AppContainer / restricted tokens / Job Objects. Higher complexity, lower
current usage. **Decision: out of scope for the first implementation**; the
`th` fleet is macOS/Linux today. Track separately.

### Failure policy when the sandbox can't be installed

Two distinct cases:

1. **Unsupported platform / old kernel** (e.g. Linux < 5.13 for Landlock, or
   a kernel without seccomp): the extension pre-dates this layer and the other
   five defenses still apply. **Fail *open* with a loud one-time `WARN`** naming
   what couldn't be applied — refusing to run every extension on an old kernel
   is worse than running with the pre-existing (still substantial) protection.
2. **Supported platform but setup *errors*** (seccomp install returns an error,
   Landlock ruleset rejected): this is a real failure of a security control we
   expected to work. **Fail *closed*** — refuse to spawn. A silent
   half-installed sandbox is the dangerous outcome.

This split mirrors the rest of the system: unknown/unsupported → degrade with
visibility; a control we *rely on* failing → deny.

## Proposed shape (implementation sketch, not built here)

- New module `extension/sandbox.rs`, feature-gated:
  `#[cfg(all(target_os = "linux", feature = "sandbox-linux"))]` and a macOS
  counterpart. Non-supported targets compile a no-op that logs the WARN once.
- A pure, testable core: `fn dangerous_syscalls() -> &'static [Syscall]` and
  `fn build_seccomp_filter() -> BpfProgram` (unit-tested: the danger set is
  present, benign syscalls like `read`/`write`/`futex`/`openat` are absent),
  plus `fn landlock_rules(paths: &SandboxPaths) -> Ruleset` (unit-tested path
  scoping). Kept pure/injected the same way th-210910's `build_child_env` /
  `verify_integrity` are, so the security-critical logic is tested without a
  spawn.
- Wiring: in `start_connection`, after `enforce_integrity` and after building
  the `Command`, install the sandbox via
  `std::os::unix::process::CommandExt::pre_exec` (seccomp filter + Landlock +
  `no_new_privs` applied in the child, post-fork/pre-exec) on Linux; via a
  `sandbox_init` call in the same `pre_exec` on macOS. Crates:
  [`seccompiler`](https://crates.io/crates/seccompiler) (Firecracker's, well
  maintained) for the BPF program, [`landlock`](https://crates.io/crates/landlock)
  for FS rules. `pre_exec` is `unsafe`; the crate forbids `unsafe_code`
  globally, so this module carries a scoped, reviewed `#[allow(unsafe_code)]`
  with the standard async-signal-safety caveat (no allocation/locks between
  fork and exec — build the BPF program *before* `pre_exec`, only load it
  inside).
- Config: an `AutoMode`-adjacent posture is overkill; a single
  `SMOOTH_EXTENSION_SANDBOX` env (`on` default on supported platforms / `off`
  escape hatch) plus the manifest opting an extension into extra path grants
  it legitimately needs (e.g. a build extension that must read `~/.cargo`).

## Consequences

- **Positive:** a compromised extension binary is contained at the kernel
  boundary — no credential-file reads, no process tampering, no
  privilege/kernel manipulation — independent of whether it routes through the
  SEP tool channel. Closes the one gap the tool-layer gate structurally cannot
  see.
- **Cost:** platform-specific `unsafe` `pre_exec` code, two new optional deps,
  a kernel-version support matrix, and CI that can only fully exercise the
  Linux path on a Linux runner (the danger-set/path-scoping *unit* tests run
  everywhere; the real-spawn enforcement test is Linux-gated).
- **Residual risk:** the denylist is not exhaustive by construction — a novel
  dangerous syscall not on the list is allowed until added. Accepted as the
  cost of not breaking interpreters; the list is the reviewed security surface
  and changes rarely. macOS parity is best-effort. Windows is unhandled.

## Open questions for review before implementation

1. Denylist confirmed over allowlist? (Recommendation: yes — see table.)
2. Landlock as the FS mechanism, degrade-to-seccomp-only under 5.13? (vs.
   requiring a minimum kernel and failing closed.)
3. The fail-open-on-unsupported / fail-closed-on-setup-error split — agreed?
4. Is the single `SMOOTH_EXTENSION_SANDBOX` on/off switch enough, or do we
   want per-mode integration with the `SMOOTH_AUTO_MODE` posture (e.g.
   `bypass` also loosens the sandbox)? Recommendation: keep them orthogonal —
   the sandbox is a containment wall, not a permission tier.
