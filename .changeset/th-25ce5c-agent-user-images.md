---
'@smooai/smooth-operator-core': minor
---

th-25ce5c: `AgentConfig::with_user_images` — stage image attachments for the current turn.

A host that received a multimodal chat turn calls `.with_user_images(images)`; `run`/`run_with_channel` then attach them to that turn's user message (via `Message::user_with_images`). Empty by default, so text-only turns are unchanged. Completes the engine side of Big Smooth's vision support (epic th-3be564); the daemon consumes it to build image turns.
