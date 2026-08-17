---
"@smooai/smooth-operator-core": patch
---

fix(rust,go,ts,python,dotnet): make a scripted mock response report the SAME usage in all five engines

The five `MockLlmProvider`s disagreed about what a scripted turn reports, so the
shared server scenario corpus in `SmooAI/smooth-operator` could not assert
`eventual_response.usage` at all — it documented five different answers instead
of one invariant:

| engine | promptTokens | completionTokens |
| --- | --- | --- |
| Go · Python · TypeScript | 0 | 0 |
| Rust | 0 | 5 |
| C# | 10 | 5 |

Rust's `0/5` was not even a mock decision: the mock reported `0/0`, and the
streaming path's "no `StreamEvent::Usage` arrived" estimator (~4 chars/token)
then invented 5 completion tokens from the scripted reply's length — so the
number moved whenever a scenario's text changed.

All five now report **10 prompt / 5 completion / 15 total** (C#'s existing
convention, the only one that was deliberate), exposed as a named helper so the
number has one definition per engine and the corpus can cite it:
`llm_provider::scripted_usage()` (Rust), `ScriptedUsage()` (Go),
`scripted_usage()` (Python), `SCRIPTED_USAGE` (TypeScript),
`MockLlmProvider.ScriptedUsage()` (C#).

**Only the FIFO scripting helpers attach it** — `push_text`/`push_tool_call` and
their per-language spellings. The benign empty reply a *drained* script falls
back to still reports nothing, so "the script ran out" stays distinguishable
from "the model answered". An explicit usage argument still overrides
(Go's `WithUsage`, Python's `usage=`, TypeScript's `usage`).

Each engine gains a unit test pinning the convention, so a drift goes red here
rather than as a confusing cross-language corpus failure downstream.
