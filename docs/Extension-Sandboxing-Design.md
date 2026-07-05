# OS-Level Extension Sandboxing — Design

> Status: **Accepted** (pearl th-a62075, the isolation follow-up to th-210910).
> Approach: **microsandbox microVM per extension.** A seccomp/Landlock
> in-process tier was considered and **dropped as over-engineering** — one
> strong, cross-platform isolation mechanism is better than two partial ones.

## Context

SEP extensions run as **host subprocesses** speaking JSON-RPC over stdio
(`ExtensionProcess::start_connection` in
`rust/smooth-operator-core/src/extension/process.rs`). They contribute tools
(`<ext>.<tool>`), event middleware, and `ui/*` dialogs to the agent.

Defenses already in place around an extension launch:

1. **Load allowlist** — `SMOOTH_EXTENSIONS_ALLOW` is default-deny.
2. **Integrity pin** (th-210910) — the binary's SHA-256 is verified against the
   manifest `[run] sha256` before spawn, on load *and* hot reload.
3. **Environment scrub** (th-210910) — `.env_clear()` + a small launch allowlist;
   ambient host secrets never inherit.
4. **Only stdio fds** — CLOEXEC on every other host fd.
5. **Per-tool permission gate** (th-d32ce6 / th-6b3ab4) — every tool call is
   classified allow/ask/deny with circuit-breakers + interactive approval.
6. **Narc surveillance + redaction** (th-5f7227 / th-10eb50).

**The gap:** all of the above constrain what the extension does *through the
SEP/tool channel*. None of it constrains what the extension **process** does
directly against the host kernel. A compromised or malicious extension binary
(supply-chain-compromised dependency, backdoored build) can still read files
outside the workspace, `ptrace` a sibling, `mount`, open raw sockets, or spawn
children that escape all tool-layer policy — none of which the tool gate sees,
because those never cross SEP.

## Decision: microsandbox microVM per extension

Run each extension inside a **microsandbox** (`msb`, v0.4.6+) microVM instead
of as a bare host subprocess. This gives hardware-virtualized isolation — a
separate guest kernel — so even a kernel-level exploit in an extension cannot
reach the host.

### Why microsandbox and not seccomp/Landlock

The earlier draft of this doc proposed a seccomp-bpf syscall denylist +
Landlock FS scoping. That was dropped:

| | microsandbox microVM | seccomp + Landlock |
|---|---|---|
| Isolation strength | Separate guest kernel — kernel exploits contained | Shared host kernel — a kernel bug escapes the filter |
| Platform | **macOS + Linux** (libkrun/HVF) — runs on the current `th` fleet, incl. the macOS dev machines | **Linux only** — can't enforce or even test on the macOS fleet |
| Network policy | Built-in egress control (`--no-net`, `--deny-domain`, `--net-rule`) | None — would need a separate mechanism |
| Team familiarity | Already integrated once (the removed Safehouse) + installed on dev machines | New surface |
| Cost | VM boot latency + a base image | Cheaper per-launch |
| Completeness | One mechanism covers syscalls + FS + network + rlimits | Two partial mechanisms, neither covering network |

The seccomp path's fatal flaw for *this* fleet: it is Linux-only, so on the
macOS dev machines it could neither enforce nor be tested — a security control
that is a no-op where most development happens is worse than one strong control
applied everywhere. microsandbox runs on macOS today (it is already installed
at `~/.microsandbox/`, v0.4.6). Maintaining a second, weaker, platform-split
tier underneath it is exactly the over-engineering we're avoiding.

### Why this is the right place for a microVM even though the VM stack was removed

The microVM dispatch stack was deleted in 2026-07 (smooth pearl th-f4a801)
because wrapping **every dispatched operative** in a per-task VM — with a
per-VM Wonk/Goalie/Narc/Scribe cast — was too heavy for the common case (your
own trusted agent doing its own work). **Extensions are the opposite case:**

- **Lower trust** — third-party code you trust *less* than your own operative.
  A microVM's cost is justified exactly where the code is untrusted.
- **Session-lived, not per-task** — an extension process persists across many
  tool calls in a session, so a single VM boot amortizes over the whole
  session instead of per dispatch.

So reintroducing a microVM *for extensions specifically* is not reversing
th-f4a801 — it applies the strong primitive precisely where its cost pays off,
while operatives keep the lighter host-subprocess + auto-mode model.

## How it plugs in

microsandbox is driven by the **`msb` CLI**, not a Rust SDK. The host launches
an extension by shelling out to `msb run` with stdio piped — the *same*
`tokio::process::Child` + stdin/stdout/stderr pipes the current direct spawn
produces, so **everything downstream (the writer/reader tasks, the JSON-RPC
framing) is unchanged.** This matters twice over:

- **operator-core stays dependency-free.** No new cargo dependency — a runtime
  shell-out to `msb`, exactly like the existing shell-out to the `smooth-dolt`
  binary. The foundational crate keeps its zero-runtime-deps posture.
- **The change is localized** to how the `Command` is built in
  `start_connection`; the protocol layer doesn't know the difference.

### The `msb run` invocation

`msb run [OPTIONS] <IMAGE> -- <command> <args…>` in attached mode connects the
guest command's stdio to our pipes. The flags that map to our isolation needs:

- `<IMAGE>` — an image carrying **both the extension's runtime and its code**.
  Declared by the manifest. A sandboxed extension ships as an image, not as
  host-mounted code: `msb run -v` in 0.4.6 has no read-only mode, and a
  *writable* bind-mount of the extension's host directory would let untrusted
  code modify host files — defeating the containment. The image is also the
  natural **integrity anchor**: a digest-pinned reference
  (`registry/ext@sha256:…`) pins exactly what runs, which is why the host-binary
  `[run] sha256` check is *skipped* on the sandboxed path (the binary runs in the
  guest from the image, not from a resolvable host path). Read-only host mounts
  for dev iteration are a follow-up if `msb` gains `:ro`.
- `-e KEY=value` — the scrubbed env allowlist + manifest `[run] env` (same set
  `build_child_env` computes today). The host env is never forwarded wholesale.
- `--no-net` by default; `--deny-domain` / an allow set from the manifest when
  the extension legitimately needs egress. **This is where OS-level network
  policy finally has a home** — the earlier doc left network entirely to the
  per-tool gate because seccomp couldn't express it; microsandbox can. The
  per-tool gate still applies on top for tool-routed calls; the VM egress
  policy bounds what the *process* can reach.
- `--rlimit`, `-u <user>` (unprivileged), `--timeout` / `--idle-timeout`.
- `--snapshot` (future) — boot from a pre-warmed snapshot to cut VM start
  latency (tracked: th-4b4544).

### Manifest schema (`[sandbox]`)

```toml
[run]
command = "node"
args = ["server.js"]

[sandbox]                      # presence + enablement opts the extension in
image = "node:20-alpine"       # required when sandboxed
memory = "512M"                # optional
cpus = 2                       # optional
network = "none"               # "none" (default) | "egress"
allow_domains = ["api.smoo.ai"]  # only when network = "egress"
```

### Configuration & failure policy

- `SMOOTH_EXTENSION_SANDBOX` env: `off` (default) → current direct host spawn,
  unchanged and non-breaking; `on` → extensions with a `[sandbox]` image run in
  a microVM.
- **Opted-in but `msb` missing / image unresolvable → fail *closed*** (refuse
  to spawn). If you asked for isolation of untrusted code, running it
  unisolated instead is the wrong answer.
- **Not opted in → direct spawn**, exactly as today. Zero behavior change for
  anyone who hasn't turned it on.

This keeps the rollout safe: the strong isolation is opt-in per deployment and
per extension, and when it *is* requested it never silently degrades.

## Consequences

- **Positive:** a compromised extension binary is contained behind a guest
  kernel — no host credential-file reads, no process tampering, no kernel
  manipulation, and egress bounded by VM policy — independent of the SEP tool
  channel. One mechanism, cross-platform, already on the fleet.
- **Cost:** VM boot latency per extension (amortized over the session;
  `--snapshot` mitigates), a base-image requirement per extension, and a
  runtime dependency on `msb` for deployments that enable it.
- **Residual:** `msb` must be present where sandboxing is enabled (that's the
  fail-closed contract, not a silent gap). Windows is not a microsandbox
  target; those deployments stay on the direct-spawn model until they move to
  Linux/macOS hosts.

## Implementation increments

1. **This PR** — manifest `[sandbox]` schema + the pure `msb run` argv builder
   (`build_msb_command`, the security-critical surface, unit-tested
   exhaustively) + config-gated wiring in `start_connection` + fail-closed
   policy. Default off, so non-breaking. A real-spawn smoke test is
   `#[ignore]`d (needs `msb` + an image; runs on macOS/Linux dev machines, not
   required in CI).
2. **Follow-up** — `--snapshot` pre-warming to cut boot latency (th-4b4544);
   the microsandbox Events API for structured guest output (th-dd84b5); a
   smooth-provided minimal base image so extensions don't each pin a public one.
