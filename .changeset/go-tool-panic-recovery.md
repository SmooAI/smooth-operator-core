---
"@smooai/smooth-operator-core": patch
---

fix(go): a panicking tool fails its own call instead of killing the process

Go escalates an unrecovered panic to a process-wide crash, and the Go core had no
`recover()` anywhere outside tests. So a buggy host tool — a nil-map write, an
index out of range — did not fail its tool call, it took down the pod and every
other live turn on it. The server-side recovery added in #504 could not help:
`recover()` only catches panics on the goroutine that deferred it, and the
panicking frames run on goroutines the core owns.

Two guards, both in `go/core/agent.go`:

- `safeExecute` wraps `tool.Execute` in the frame that calls it, turning a panic
  into an ordinary `IsError` tool result the model can react to (stack goes to the
  process log, never into the model-visible content). It has to live there rather
  than in `Run`/`RunStream`, because `ParallelToolCalls` dispatches each tool on
  its own goroutine — no caller-side guard can reach those. This matches
  TypeScript, Python and .NET core, whose `catch` around `execute()` already
  converted a throwing tool into an error result.
- The goroutine `RunStream` spawns now recovers as a backstop, so a panic
  elsewhere in the turn (a hook, a checkpoint store, the streaming client) is
  reported through the documented `Stream` error contract — channel closed without
  a `StreamDone`, reason in `Err()` — instead of crashing the process. That is
  what Rust already gets for free by confining a turn to its `JoinHandle`.

Rust, TypeScript, Python and .NET were checked and need no change; Go was the only
engine with the gap, and neither .NET nor TypeScript core has a fire-and-forget
task that could hide the same hole.
