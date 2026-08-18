---
'@smooai/smooth-operator-core': minor
---

Ask the gateway for usage on every streaming request (`stream_options: {"include_usage": true}`), in all
five engines.

The OpenAI streaming API **omits usage unless it is explicitly requested**, and nothing here ever
requested it. So the missing usage chunk was never the gateway losing data — it was the gateway
correctly honouring a request that never asked. Everything built on top of that misattribution
compensates for one unset request parameter: two char-count estimators, `prompt_tokens` hardcoded to
`0`, `completion_tokens = content.len() / 4`, the `usage_estimated` and `cost_estimated` flags, and a
cross-language "is this measured?" convention.

Verified against llm.smoo.ai (LiteLLM 1.95.0, `groq-gpt-oss-120b`), same prompt both ways:

- **without** the field — 7 chunks, **0** carrying usage
- **with** the field — 8 chunks, **1** carrying `"prompt_tokens": 73, "completion_tokens": 8`

Sent only when streaming: it is meaningless otherwise, and leaving it off keeps a non-streaming
request byte-identical to before. Python honours an explicit caller-supplied `stream_options` rather
than overriding it.

Not fixed by this, and worth knowing: the per-request **cost** headers are present on a streamed
response but all read `0.0`, because at header-flush time the completion has not been priced yet.
`parse_gateway_cost` already maps `0` to `None`, so gateway cost stays unavailable on the streaming
path and the `response_id` → `LiteLLM_SpendLogs.request_id` join remains the only authoritative
per-turn cost there.
