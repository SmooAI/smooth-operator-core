---
'@smooai/smooth-operator-core': patch
---

fix(go): a truncated stream is an error, not a clean finish; SEP hooks run on RunStream; no goroutine/socket leak

Three defects on the Go engine's streaming path, all of which the Rust reference
already handled. The first was silent data loss.

**1. Truncated streams were reported as completed turns.** `NewGatewayClient` set
`http.Client{Timeout: 60s}`, and in Go that timeout covers reading the response
BODY — so it tore the SSE body out mid-stream. `ChatStream` then never called
`scanner.Err()`: the scan loop simply ended, the channel closed normally, and the
agent emitted `StreamDone` with the partial text and `Stream.Err() == nil`. A
reasoning or multi-tool turn running past 60s returned a half-sentence that was
appended to history and checkpointed as the assistant turn, with nothing anywhere
recording a failure. The same swallow applied to any mid-stream reset and to a
single SSE line over the scanner's buffer ceiling.

The overall timeout is now 600s, mirroring the reference's reqwest client, and
liveness is enforced where it belongs — a 60s **per-read idle** watchdog, matching
the reference's `CHUNK_IDLE_TIMEOUT`, which measures the gap between reads so a
slow-but-progressing stream is never cut off. `ChatChunk` gained an `Err` field
(the Go analogue of the reference's `Result<StreamEvent>` channel item); a stream
that ends any way other than cleanly now delivers it, and `runStream` aborts the
turn: no `StreamDone`, non-nil `Err()`, and the partial text is neither appended to
the thread nor checkpointed. The line ceiling went 1MB → 8MB, and overrunning it is
now loud.

**2. `runStream` bypassed every SEP hook.** It called no `sepDispatch`, no
`sepToolCallPlan`, no `sepTurnComplete` — so the extension `tool_call` veto was
enforced on `Run` and not on `RunStream`, which is to say not enforced, and
extensions saw no `turn_start`/`message_end`/`turn_end` for any streamed turn. The
streaming loop now runs the same fold as `Run`: vetoed calls never dispatch and
their reason becomes the tool result, argument rewrites reach both the tool and the
emitted `tool_call` event.

**3. Goroutine and socket leak.** Every send — the cost chunk in `ChatStream`, and
every event in `runStream` onto an unbuffered channel — was a bare channel send, so
a consumer that broke out of `range s.Events()` early parked the producer forever
holding its socket. Sends now select on the turn context, `RunStream` owns a
cancellable context, and `Stream` gained a `Close()`: Go gives a sender no way to
notice a receiver has walked away (unlike the reference's `tx.send().is_err()`), so
`Close` is that signal. Cancelling the caller's own context works too. The
`MockLlmProvider` honours its context for the same reason.

Covered by `go/core/stream_truncation_test.go` — torn body, idle stall, client-timeout
expiry, over-long line, no-checkpoint-on-truncation, healthy long turn, SEP veto /
rewrite / turn events, and goroutine-stack leak assertions on both abandon paths.
Each was confirmed to fail against the unfixed code.
