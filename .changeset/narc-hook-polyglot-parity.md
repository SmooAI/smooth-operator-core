---
'@smooai/smooth-operator-core': patch
---

Port the NarcHook secret-detection + prompt-injection scanner to the Go, TypeScript, Python and .NET engines.

The scanner was Rust-only, which meant the extension boundary in the other four engines passed tool-call arguments to a subprocess unscanned and handed the subprocess's result back to the model verbatim — no check for leaked credentials, no check for injection payloads. Each engine now installs the same `ToolHook`:

- **`pre_call`** scans the arguments. A Block-severity injection match (the active data/URL exfiltration signals) blocks the call before the tool runs. Lower-severity injection and any secret are alerted, not blocked — a tool argument legitimately carrying a secret is common enough that a hard block there would be a footgun.
- **`post_call`** scans the result and **redacts**: a secret in a tool result raises a Block alert and is rewritten to `[REDACTED:<pattern-name>]` in place, so the model never sees the raw credential. Injection in a result stays surveillance-only and is never rewritten.

Detection is 10 credential patterns (AWS keys, private keys, bearer tokens, provider keys, Stripe keys, …) and 8 injection patterns (instruction override, role hijack, system-prompt spoofing, jailbreak, base64 smuggling, data/URL exfiltration, suspicious hosts).

Because a security hook that is weaker in one language is a real gap, the detection set is now pinned by a shared corpus at `spec/narc/corpus.json` — 39 vectors of positive/near-miss pairs, generated from the Rust reference. All five engines (Rust included) replay it and assert identical findings, in identical order, at identical severities, so dropping a pattern or downgrading a severity fails that language's test suite instead of silently shipping.
