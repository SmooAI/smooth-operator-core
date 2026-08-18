---
'@smooai/smooth-operator-core': minor
---

Carry cost/usage provenance and the gateway response id out of the engine, so nothing downstream can
publish a fabricated number as a measurement.

`collect_stream` invents a usage struct whenever a response carries no usage chunk — which LiteLLM
does for every `smooth-*` alias — hardcoding `prompt_tokens = 0` and estimating `completion_tokens`
as `content.len() / 4`. The estimate is kept, because budget enforcement needs something to multiply,
but it is now flagged: `LlmResponse.usage_estimated`, aggregated onto `AgentEvent::Completed` as
`usage_estimated`. This is what let production record `input_tokens = 0` on every streamed turn
beside an output count that only ever tracked the reply's length. The old comment there claimed the
estimate would "produce a real cost number against ModelPricing"; it does not, and it now says so.

`AgentEvent::Completed` also gains `cost_estimated` — true once any call in the run was priced from
the local `ModelPricing` table instead of the gateway's own figure. That table cannot price aliased
routes and returns the free tier for anything it does not recognise, so a tainted total may be a wild
under-count while looking exact. Cost and usage provenance are tracked separately on purpose: the
gateway reports cost on an HTTP header and usage on an SSE chunk, so either can be authoritative
while the other is a guess.

Finally, the gateway's response id (`chatcmpl-…`, or `msg_…` on the Anthropic-native path) is now
captured off both the streaming and non-streaming paths and surfaced as `LlmResponse.response_id` and
`AgentEvent::Completed.response_id`. It was previously discarded at deserialization. It is the join
key to `LiteLLM_SpendLogs.request_id`, whose row carries the gateway's authoritative dollars **and**
its real prompt/completion counts — the only trustworthy source for either while the flags above can
be set.

All three fields cross the Temporal activity boundary via `ModelCallOutput`, so a durable replay
cannot silently launder an estimate back into a measurement. `smooth-operator-temporal`'s core
requirement moves 1.8 -> 1.9 accordingly.

Note for consumers matching exhaustively on `StreamEvent`: this adds a `ResponseId` variant.
