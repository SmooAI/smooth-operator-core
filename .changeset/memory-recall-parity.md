---
"@smooai/smooth-operator-core": patch
---

fix(all): make memory auto-recall byte-identical across the five cores (th-ffaeae)

The five engines all recall memories into a turn's context, and all five did it
differently. What looked like "three spellings of one header" turned out to be a
divergence at every layer of the feature:

|  | Rust | C# | Python / Go / TS |
| --- | --- | --- | --- |
| header | `[Recalled memories]` | `Relevant memory:` | `Relevant memory (things you remember…):` |
| entry line | `- (Type, relevance=N.NN): text` | `- text` | `- text` |
| freshness note | **yes** | no | no |
| score | matched ÷ query words | raw overlap count | raw overlap count |
| tokens | whitespace split | alphanumeric, **drops length ≤ 2** | alphanumeric |
| top-k | **5, hardcoded, no knob** | 4 | 4 |

So the same memories in the same store produced a different prompt on every
engine — which is why no shared conformance scenario could be written for the
feature, and why an agent's recall quality silently depended on which core it
happened to run on.

All five now emit the same block, verified by generating it from each engine on
identical input and comparing hashes (one hash, five languages):

```
[Recalled memories]
Note: 'the memory says X exists' is not the same as 'X exists now'. Before recommending…
- (Project, relevance=0.57): the retry lives in fetch.rs
- (User, relevance=0.14): brent prefers execution over questions
```

Three of the unified behaviours are real fixes rather than cosmetic alignment:

- **The verify-before-recommend note now reaches every engine.** Only Rust told
  the model that a memory naming a function, file, or flag is a claim about the
  *past*; on the other four it happily recommended symbols deleted three releases
  ago. It is still emitted only for time-sensitive kinds (`Project`/`Reference`),
  so it stays signal rather than boilerplate the model learns to skip.
- **Punctuation no longer defeats a match.** Rust scored by substring against
  whitespace-split words, so a query ending `…my name?` never matched a memory
  containing `name` and the entry was silently not recalled. Scoring is now
  tokenised in every core. (C# additionally dropped every token of length ≤ 2 —
  correct for knowledge retrieval, wrong here — so `my` could never contribute.)
- **Relevance is normalised to 0–1** (a fraction of the query's distinct tokens)
  rather than a raw overlap count, because it is rendered into the prompt and a
  bare count is comparable neither between entries nor between engines.

Rust also gains the `memory_top_k` knob the other four already had — its `5` was
hardcoded at the call site — and `5` becomes the shared default everywhere.

**Behaviour change to be aware of:** the recall block's text is different on all
five engines, and slightly more is recalled by default on four of them (5 rather
than 4). Anything asserting the old `Relevant memory` header needs updating; the
header is now exported as a constant (`RECALL_HEADER` / `RecallHeader`) so tests
can reference it instead of hardcoding.

Rendering and scoring moved out of each agent into the memory module
(`render_recall_block` / `renderRecallBlock` / `RenderRecallBlock` /
`MemoryRecall.RenderRecallBlock`) and are pinned by tests named after their Rust
counterparts, so the next drift fails a test instead of going unnoticed.
