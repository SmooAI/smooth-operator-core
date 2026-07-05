---
'@smooai/smooth-operator-core': patch
---

th-a62075: add the OS-level extension sandboxing design doc (`docs/Extension-Sandboxing-Design.md`).

Design pass for the platform-specific follow-up to th-210910 — documents the
threat model (a compromised extension binary issuing arbitrary syscalls the
SEP/tool layer never sees), the decisions (seccomp-bpf **denylist** of
dangerous syscalls over an allowlist that would break Node/Python; Landlock
FS scoping degrading to seccomp-only under kernel 5.13; `no_new_privs` always,
uid-drop only when privileged; network deliberately left to the per-tool
permission gate), macOS `sandbox_init` parity, the fail-open-on-unsupported /
fail-closed-on-setup-error split, and an implementation sketch hooking
`pre_exec` in `extension/process.rs`. No code yet — implementation tracked on
the same pearl.
