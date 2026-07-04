//! `sep-echo-peer` — a minimal, dependency-free SEP extension in Rust.
//!
//! Reads JSON-RPC 2.0 frames as ndjson on stdin and replies on stdout. It is
//! the fixture-replay peer for the engine host's process-lifecycle tests (so
//! that suite needs no Node runtime in CI), and doubles as the smallest
//! possible reference SEP extension. Behavior mirrors the spec's `echo.mjs`:
//! handshake, answer `ping`, continue every `hook`, echo the `say` tool, and
//! exit on `shutdown`.
//!
//! Three env-gated test modes:
//! - `SEP_ECHO_BLOCK=1` — every `hook` vetoes (the tool_call-layering test).
//! - `SEP_ECHO_HANG=1` — every `hook` hangs forever (never replies), driving the
//!   fail-closed timeout path: the host times out, `$/cancel`s, and blocks.
//! - `SEP_ECHO_SLOW=1` — `tool/execute` streams a `tool/update` progress
//!   notification and then WITHHOLDS its reply until a `$/cancel` arrives for
//!   that request, at which point it answers -32800 Cancelled. Exercises the
//!   tool/update + $/cancel wire path deterministically.
//! - `SEP_ECHO_UI=1` — `tool/execute` sends an ext→host `ui/request` (a
//!   `confirm`) whose prompt echoes the `ui_capabilities` the host declared at
//!   `initialize`, waits for the host's reply, and returns `confirmed=<…>` as
//!   the tool content. Exercises the ui/request seam and proves ui_capabilities
//!   threading end-to-end (answered value or `error:-32001` when headless).
//!
//! Phase 8 modes:
//! - `SEP_ECHO_SYSPROMPT=1` — the `before_agent_start` hook rewrites the system
//!   prompt to `REWRITTEN BY ECHO` (declares the `before_agent_start` hook).
//! - `SEP_ECHO_CTX=1` — the `context` hook replaces the whole message array with
//!   a single user message `CONTEXT REPLACED` (declares the `context` hook).
//! - `SEP_ECHO_BUS_PUB=1` — `tool/execute` sends a `bus/publish` (topic `ping`)
//!   before replying, driving the inter-extension bus.
//! - `SEP_ECHO_BUS_SUB=1` — subscribes to `bus/event` and, on receipt, records
//!   the topic via `kv/set(bus_seen, <topic>)` so a test can observe the fanout.

use std::io::{BufRead, Write};

use serde_json::{json, Value};

fn main() {
    let stdin = std::io::stdin();
    let stdout = std::io::stdout();
    let mut out = stdout.lock();

    let slow = std::env::var("SEP_ECHO_SLOW").is_ok();
    let ui = std::env::var("SEP_ECHO_UI").is_ok();
    // `SEP_ECHO_PROVIDER=1` — register an LLM provider and answer `provider/*`
    // (Phase 7). `provider/complete` streams two `provider/delta` chunks ("Hel",
    // "lo") when `stream` is set, then replies "Hello"; `provider/oauth_login`
    // returns a canned credential bundle. Exercises the proxied-streaming +
    // OAuth wire deterministically.
    let provider = std::env::var("SEP_ECHO_PROVIDER").is_ok();
    // `SEP_ECHO_ENV=1` — report a slice of the child's *observed* environment in
    // the initialize result so an integration test can prove the host scrubbed
    // the ambient host env (th-210910): a host-side secret must be ABSENT while
    // PATH (allow-listed) and the manifest's own vars pass through.
    let env_report = std::env::var("SEP_ECHO_ENV").is_ok();
    // In slow mode, the JSON-RPC id of a `tool/execute` whose reply we are
    // holding back until a matching `$/cancel` arrives.
    let mut held_tool_call: Option<Value> = None;
    // ui_capabilities the host declared at `initialize`; SEP_ECHO_UI echoes them
    // back through the ui/request prompt so a test can prove they were threaded.
    let mut ui_caps: Vec<String> = Vec::new();

    let mut lines = stdin.lock().lines();
    while let Some(line) = lines.next() {
        let Ok(line) = line else { break };
        if line.trim().is_empty() {
            continue;
        }
        let Ok(msg): Result<Value, _> = serde_json::from_str(&line) else {
            continue;
        };

        // A frame with an id but no method is the host's *response* to one of our
        // fire-and-forget ext→host requests (kv/set, bus/publish). Ignore it —
        // the request sites that need a reply read it inline (UI mode).
        if msg.get("method").is_none() {
            continue;
        }

        let method = msg.get("method").and_then(Value::as_str).unwrap_or("");
        let params = msg.get("params").cloned().unwrap_or(Value::Null);

        // Notifications (no id). `$/cancel` releases a held tool/execute;
        // `bus/event` (Phase 8) is the inter-extension bus fanout.
        let Some(id) = msg.get("id").cloned() else {
            if method == "$/cancel" {
                if let Some(held) = held_tool_call.take() {
                    write_frame(&mut out, &error(&held, -32800, "cancelled"));
                }
            } else if method == "event" && params.get("event").and_then(Value::as_str) == Some("bus/event") {
                // Observe envelope: params = { event, seq, context, payload }.
                // The bus payload is { from, topic, payload }.
                let topic = params.get("payload").and_then(|p| p.get("topic")).and_then(Value::as_str).unwrap_or("");
                write_frame(
                    &mut out,
                    &json!({ "jsonrpc": "2.0", "id": 7002, "method": "kv/set", "params": { "key": "bus_seen", "value": topic } }),
                );
            } else if method == "event" && params.get("event").and_then(Value::as_str) == Some("widget/key") {
                // Phase 8: a targeted host→ext render-block v2 keypress. Record
                // the key so a test can prove targeted routing (no subscription).
                let key = params.get("payload").and_then(|p| p.get("key")).and_then(Value::as_str).unwrap_or("");
                write_frame(
                    &mut out,
                    &json!({ "jsonrpc": "2.0", "id": 7003, "method": "kv/set", "params": { "key": "widget_key", "value": key } }),
                );
            }
            continue;
        };

        let reply = match method {
            "initialize" => {
                let their_version = params.get("protocol_version").and_then(Value::as_u64).unwrap_or(1);
                if let Some(caps) = params.get("ui_capabilities").and_then(Value::as_array) {
                    ui_caps = caps.iter().filter_map(|v| v.as_str().map(String::from)).collect();
                }
                // Declare the intercept hooks this mode handles so the host's
                // `any_hook` gate is exact (Phase 8). An undeclared/empty list
                // means "unknown" and the host runs every hook to be safe.
                let mut hooks: Vec<&str> = Vec::new();
                if std::env::var("SEP_ECHO_BLOCK").is_ok() || std::env::var("SEP_ECHO_HANG").is_ok() {
                    hooks.push("tool_call");
                }
                if std::env::var("SEP_ECHO_PATCH").is_ok() {
                    hooks.push("tool_result");
                }
                if std::env::var("SEP_ECHO_SYSPROMPT").is_ok() {
                    hooks.push("before_agent_start");
                }
                if std::env::var("SEP_ECHO_CTX").is_ok() {
                    hooks.push("context");
                }
                let mut subs = vec!["turn_start", "turn_end", "message_end", "session_start", "session_shutdown"];
                if std::env::var("SEP_ECHO_BUS_SUB").is_ok() {
                    subs.push("bus/event");
                }
                success(
                    &id,
                    json!({
                        "protocol_version": their_version.min(1),
                        "extension": { "name": "echo", "version": "0.1.0" },
                        // th-210910: not part of the SEP InitializeResult schema (the
                        // host ignores it); an integration test reads it off the raw
                        // reply to prove the child env was scrubbed.
                        "env_report": if env_report {
                            json!({
                                "AWS_SECRET_ACCESS_KEY": std::env::var("AWS_SECRET_ACCESS_KEY").ok(),
                                "PATH_present": std::env::var("PATH").is_ok(),
                                "SEP_ECHO_ENV": std::env::var("SEP_ECHO_ENV").ok(),
                            })
                        } else { Value::Null },
                        "registrations": {
                            "tools": [{
                                "name": "say",
                                "description": "Echo a phrase back.",
                                "parameters": { "type": "object", "properties": { "phrase": { "type": "string" } }, "required": ["phrase"] }
                            }],
                            "commands": [{ "name": "echo-cmd", "description": "Echo a slash-command back." }],
                            "shortcuts": [{ "key": "ctrl+e", "command": "echo-cmd", "description": "Run echo-cmd" }],
                            "providers": if provider {
                                json!([{
                                    "name": "echo-provider",
                                    "base_url": "https://echo.example/v1",
                                    "api_key_env": "ECHO_KEY",
                                    "oauth": true,
                                    "models": [{ "id": "echo-1", "display_name": "Echo One" }]
                                }])
                            } else { json!([]) },
                            "message_renderers": if std::env::var("SEP_ECHO_RENDERER").is_ok() {
                                json!([{ "tag": "echo_card", "template": { "kind": "markdown", "text": "**{{title}}**" } }])
                            } else { json!([]) },
                            "hooks": hooks,
                            "subscriptions": subs
                        }
                    }),
                )
            }
            "ping" => success(&id, json!({})),
            // `SEP_ECHO_HANG=1` makes every `hook` hang forever (never replies).
            // Drives the fail-closed timeout path: the host must time out, send
            // `$/cancel`, and BLOCK the tool without stalling the turn.
            "hook" if std::env::var("SEP_ECHO_HANG").is_ok() => continue,
            // `SEP_ECHO_PATCH=1` rewrites `tool_result` content via a Modify
            // outcome (and continues `tool_call`) — the tool_result-hook test.
            "hook" if std::env::var("SEP_ECHO_PATCH").is_ok() => {
                if params.get("hook").and_then(Value::as_str) == Some("tool_result") {
                    success(&id, json!({ "action": "modify", "patch": { "content": "[patched by echo]" } }))
                } else {
                    success(&id, json!({ "action": "continue" }))
                }
            }
            // `SEP_ECHO_BLOCK=1` turns the peer into a veto gate — used by the
            // host's tool_call-layering test.
            "hook" if std::env::var("SEP_ECHO_BLOCK").is_ok() => success(&id, json!({ "action": "block", "reason": "blocked by echo peer" })),
            // Phase 8: `before_agent_start` rewrites the system prompt.
            "hook" if std::env::var("SEP_ECHO_SYSPROMPT").is_ok() && params.get("hook").and_then(Value::as_str) == Some("before_agent_start") => {
                success(&id, json!({ "action": "modify", "patch": { "system_prompt": "REWRITTEN BY ECHO" } }))
            }
            // Phase 8: `context` replaces the entire message array.
            "hook" if std::env::var("SEP_ECHO_CTX").is_ok() && params.get("hook").and_then(Value::as_str) == Some("context") => success(
                &id,
                json!({ "action": "modify", "patch": { "messages": [{ "role": "user", "content": "CONTEXT REPLACED" }] } }),
            ),
            "hook" => success(&id, json!({ "action": "continue" })),
            // UI mode: round-trip a ui/request confirm to the host, echoing the
            // negotiated caps in the prompt, and return the host's answer.
            "tool/execute" if ui => {
                let req_id = json!(9001);
                write_frame(
                    &mut out,
                    &json!({
                        "jsonrpc": "2.0", "id": req_id, "method": "ui/request",
                        "params": { "kind": "confirm", "prompt": format!("caps={}", ui_caps.join(",")) }
                    }),
                );
                // Read frames until the host's reply to req_id arrives.
                let mut confirmed = String::from("no-reply");
                for inner in lines.by_ref() {
                    let Ok(inner) = inner else { break };
                    let Ok(v): Result<Value, _> = serde_json::from_str(&inner) else { continue };
                    if v.get("id") == Some(&req_id) {
                        confirmed = if let Some(err) = v.get("error") {
                            format!("error:{}", err.get("code").and_then(Value::as_i64).unwrap_or(0))
                        } else {
                            v.get("result")
                                .and_then(|r| r.get("confirmed"))
                                .and_then(Value::as_bool)
                                .map_or_else(|| "none".to_string(), |b| b.to_string())
                        };
                        break;
                    }
                }
                success(&id, json!({ "content": format!("confirmed={confirmed}") }))
            }
            // Slow mode: stream progress, then hold the reply for a $/cancel.
            "tool/execute" if slow => {
                let call_id = params.get("call_id").and_then(Value::as_str).unwrap_or("");
                write_frame(
                    &mut out,
                    &notification("tool/update", json!({ "call_id": call_id, "message": "started", "progress": 0.0 })),
                );
                held_tool_call = Some(id);
                continue;
            }
            // Phase 8: publish onto the inter-extension bus before replying.
            "tool/execute" if std::env::var("SEP_ECHO_BUS_PUB").is_ok() => {
                write_frame(
                    &mut out,
                    &json!({ "jsonrpc": "2.0", "id": 7001, "method": "bus/publish", "params": { "topic": "ping", "payload": { "n": 1 } } }),
                );
                success(&id, json!({ "content": "published" }))
            }
            "tool/execute" => {
                let phrase = params.get("arguments").and_then(|a| a.get("phrase")).and_then(Value::as_str).unwrap_or("");
                success(&id, json!({ "content": phrase }))
            }
            // Provider proxy: stream two delta chunks (when asked) then reply
            // with the assembled content. `request_id` keys the delta stream.
            "provider/complete" => {
                let request_id = params.get("request_id").and_then(Value::as_str).unwrap_or("").to_string();
                let stream = params.get("stream").and_then(Value::as_bool).unwrap_or(false);
                if stream {
                    for chunk in ["Hel", "lo"] {
                        write_frame(
                            &mut out,
                            &notification(
                                "provider/delta",
                                json!({ "request_id": request_id, "event": { "type": "Delta", "content": chunk } }),
                            ),
                        );
                    }
                }
                success(
                    &id,
                    json!({
                        "content": "Hello",
                        "finish_reason": "stop",
                        "usage": { "prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5 },
                        "resolved_model": "echo-1"
                    }),
                )
            }
            // OAuth: the extension would drive a real handshake (ui/* callbacks);
            // the peer just returns a canned bundle so the wire is exercised.
            "provider/oauth_login" => success(
                &id,
                json!({ "api_key": "sk-echo-oauth", "refresh_token": "rt-echo", "expires_at": 1_900_000_000i64 }),
            ),
            "provider/oauth_refresh" => {
                let rt = params.get("refresh_token").and_then(Value::as_str).unwrap_or("");
                success(&id, json!({ "api_key": "sk-echo-refreshed", "refresh_token": rt }))
            }
            // A command handler echoes its name + args back as content. The
            // command-tier context arrives in params.context (the host minted it).
            "command/execute" => {
                let cmd = params.get("command").and_then(Value::as_str).unwrap_or("");
                let args = params.get("arguments").cloned().unwrap_or(Value::Null);
                success(&id, json!({ "content": format!("ran {cmd} {args}") }))
            }
            // One static completion so the round-trip is observable.
            "command/complete" => {
                let partial = params.get("partial").and_then(Value::as_str).unwrap_or("");
                success(
                    &id,
                    json!({ "completions": [{ "value": format!("{partial}-done"), "description": "echo completion" }] }),
                )
            }
            "shutdown" => {
                write_frame(&mut out, &success(&id, json!({})));
                std::process::exit(0);
            }
            other => error(&id, -32601, &format!("method not found: {other}")),
        };
        write_frame(&mut out, &reply);
    }
}

fn notification(method: &str, params: Value) -> Value {
    json!({ "jsonrpc": "2.0", "method": method, "params": params })
}

fn success(id: &Value, result: Value) -> Value {
    json!({ "jsonrpc": "2.0", "id": id, "result": result })
}

fn error(id: &Value, code: i64, message: &str) -> Value {
    json!({ "jsonrpc": "2.0", "id": id, "error": { "code": code, "message": message } })
}

fn write_frame(out: &mut impl Write, frame: &Value) {
    if let Ok(mut s) = serde_json::to_string(frame) {
        s.push('\n');
        let _ = out.write_all(s.as_bytes());
        let _ = out.flush();
    }
}
