---
"@smooai/smooth-operator-core": patch
---

fix(all cores): request `stream_options.include_usage` on streaming calls so token counts survive

A streaming response from the gateway carries NO token usage unless the request sets
`stream_options: {include_usage: true}` — the gateway only appends the trailing usage
chunk when asked. The JS SDK sets this implicitly, so the TypeScript core reported
tokens; the raw HTTP clients in the Rust, Go, Python, and .NET cores never set it, so
their streaming turns reported zero prompt/completion tokens. Downstream that made
`eventual_response.usage` empty and every per-turn cost read $0 (th-58db12). All four
raw-HTTP cores now set it; the existing trailing-chunk capture does the rest.
