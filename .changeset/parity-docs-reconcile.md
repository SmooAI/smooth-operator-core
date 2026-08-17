---
'@smooai/smooth-operator-core': patch
---

Reconcile the parity documentation with what is actually merged.

Each parity workstream updated the feature list as it landed, which left the list accurate line-by-line but wrong in aggregate: multimodal images were still listed as "still being ported" after they shipped in all five, and seven capabilities that are in all five engines today (multimodal input, the tool-hook lifecycle, the permission gate + deny-policy + grants, the SEP extension host, and gateway cost headers) were missing from the list entirely.

Every claim in the revised list was re-verified against merged code by symbol, not by reading the PR that added it. The two remaining honest exceptions are now stated up front in the README rather than buried: the extension **sandbox / integrity hardening** is Rust-first (capability declarations are honoured in all five; process-level confinement and manifest-integrity verification are not), and the durable-execution **backend** ships only for Rust while the `AgentExecutor` seam it plugs into is in all five.

Also documents the three shared corpora — eval scenarios, the Narc detection set, and the provider-routing table — as the mechanism behind the parity claim: each is generated from the Rust reference and replayed by all five engines including Rust, so the reference cannot drift away from its own ports unnoticed.
