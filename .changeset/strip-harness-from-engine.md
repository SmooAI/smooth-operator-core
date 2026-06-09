---
'@smooai/smooth-operator-core-monorepo': minor
---

Strip the th-code coding-harness and Big-Smooth concepts out of the public engine so `smooai-smooth-operator-core` is a clean, generic agent engine (Rust crate bumped 0.13.7 → 0.14.0, breaking).

Removed:

- `coding_workflow` module (the `th code` coding workflow)
- `skills` module + the `create-skill` builtin skill (SKILL.md discovery, Sandbox/Host scope)
- `bigsmooth_client` module, the `bigsmooth` cargo feature, and the gated reporter/steering hooks wired into the agent loop (`with_reporter`/`report_to_bigsmooth`/`check_steering`/`ReporterEvent`/`ControlEvent`)
- The coding-harness cast roles `fixer` and `oracle` and their prompt files, plus the two harness routers that only dispatched between them (`chief`, `intent_classifier`)

The generic engine surface (Agent/AgentConfig/AgentEvent, LLM client/provider, Tool/ToolRegistry, Checkpoint, Conversation/Message, Memory, KnowledgeBase, CostTracker, Workflow, providers/routing, and the `cast` mechanism with its generic roles tagger/presser/recapper/mapper/heckler/scout/runner) is unchanged. Engine docs de-branded to drop microVM/sandbox/Big-Smooth framing.
