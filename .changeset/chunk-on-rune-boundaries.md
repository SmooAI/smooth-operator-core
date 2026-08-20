---
'@smooai/smooth-operator-core': patch
---

go/typescript: split streamed mock text on **character** boundaries, not bytes / UTF-16 code units.

`MockLlmProvider.ChatStream` splits a scripted reply into a few content deltas. Go sliced the string by **bytes**, so any multi-byte character straddling a chunk boundary was cut in half and each fragment became invalid UTF-8 — which `json.Marshal` replaces with `U+FFFD`. That is permanent: once a chunk is serialized, reassembling the pieces no longer recovers the text. TypeScript slices a JS string, i.e. **UTF-16 code units** — safe for the Basic Multilingual Plane (em-dashes, accents, CJK) but not for astral characters, where an emoji can be cut between its surrogate pair and encode to `U+FFFD` the same way.

Found via the polyglot server conformance corpus (pearl th-ef78d0): `interaction-choices-park-resume` streams `"Pro it is — pulling that quote up."`, which is 36 bytes, so 3 parts put a boundary at byte 12 — the middle of the em-dash — and the Go server's accumulated tokens came back as `"Pro it is ��� pulling that quote up."`. The sibling scenario's reply is 33 bytes and its boundaries happen to miss every rune, which is why only one scenario ever tripped and why this survived so long: with ASCII-only fixtures it is invisible.

Both the text chunker and the **tool-call `arguments`** midpoint split are fixed in each language, since arguments are JSON and carry non-ASCII values (`{"city":"München"}`).

Python (slices `str` by code point), Rust (replays an explicit `Vec<StreamEvent>`, never splits) and .NET (delegates to `ToChatResponseUpdates()`) were checked and are **not** affected.

Regression tests cover an em-dash, an emoji (surrogate pair) and accents + CJK, asserting each chunk is valid **individually** — concatenating the pieces heals the corruption in memory and would hide the bug.
