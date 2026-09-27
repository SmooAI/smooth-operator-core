---
"@smooai/smooth-operator-core": patch
---

fix(rust): stop replaying `reasoning_content` to Groq models (SMOODEV-3342)

The Rust engine replays `reasoning_content` on every assistant history message on
purpose: DeepSeek and Kimi thinking-mode upstreams 400 without it ("reasoning_content
must be passed back in the thinking mode", th-eae0f8). Groq does the opposite and 400s
**with** it:

```
GroqException … 'messages.2' : for 'role:assistant' … property 'reasoning_content' is unsupported
```

Behind a LiteLLM gateway that 400 does not surface — the gateway falls back to another
model. So every tool-using turn on `groq-gpt-oss-120b` (among others, the
`smooai-gateway` preset's judge slot) was served by the fallback model from the second
iteration on, while looking healthy.

`LlmClient::build_openai_request`, the body builder shared by the streaming and
non-streaming OpenAI-compatible paths, now strips `reasoning_content` from every message
when the configured model is a Groq model. That covers both prior-turn history and
in-turn tool iterations. The new `llm::rejects_replayed_reasoning(model)` predicate
matches the gateway alias form (`groq-…`) and the LiteLLM provider form (`groq/…`),
case-insensitively. It is deliberately model-gated: every other model — DeepSeek and
Kimi included — still gets the replay exactly as before.

The Anthropic-native request path never sent `reasoning_content` and is unchanged. The
TypeScript, Python, Go and .NET engines never replay reasoning — their assistant
history messages carry only `role`, `content` and `tool_calls` — so they have nothing to
strip and are unchanged.
