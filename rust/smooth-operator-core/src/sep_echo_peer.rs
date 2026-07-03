//! `sep-echo-peer` — a minimal, dependency-free SEP extension in Rust.
//!
//! Reads JSON-RPC 2.0 frames as ndjson on stdin and replies on stdout. It is
//! the fixture-replay peer for the engine host's process-lifecycle tests (so
//! that suite needs no Node runtime in CI), and doubles as the smallest
//! possible reference SEP extension. Behavior mirrors the spec's `echo.mjs`:
//! handshake, answer `ping`, continue every `hook`, echo the `say` tool, and
//! exit on `shutdown`.
//!
//! Two env-gated test modes:
//! - `SEP_ECHO_BLOCK=1` — every `hook` vetoes (the tool_call-layering test).
//! - `SEP_ECHO_SLOW=1` — `tool/execute` streams a `tool/update` progress
//!   notification and then WITHHOLDS its reply until a `$/cancel` arrives for
//!   that request, at which point it answers -32800 Cancelled. Exercises the
//!   tool/update + $/cancel wire path deterministically.

use std::io::{BufRead, Write};

use serde_json::{json, Value};

fn main() {
    let stdin = std::io::stdin();
    let stdout = std::io::stdout();
    let mut out = stdout.lock();

    let slow = std::env::var("SEP_ECHO_SLOW").is_ok();
    // In slow mode, the JSON-RPC id of a `tool/execute` whose reply we are
    // holding back until a matching `$/cancel` arrives.
    let mut held_tool_call: Option<Value> = None;

    for line in stdin.lock().lines() {
        let Ok(line) = line else { break };
        if line.trim().is_empty() {
            continue;
        }
        let Ok(msg): Result<Value, _> = serde_json::from_str(&line) else {
            continue;
        };

        let method = msg.get("method").and_then(Value::as_str).unwrap_or("");
        let params = msg.get("params").cloned().unwrap_or(Value::Null);

        // Notifications (no id). `$/cancel` releases a held tool/execute.
        let Some(id) = msg.get("id").cloned() else {
            if method == "$/cancel" {
                if let Some(held) = held_tool_call.take() {
                    write_frame(&mut out, &error(&held, -32800, "cancelled"));
                }
            }
            continue;
        };

        let reply = match method {
            "initialize" => {
                let their_version = params.get("protocol_version").and_then(Value::as_u64).unwrap_or(1);
                success(
                    &id,
                    json!({
                        "protocol_version": their_version.min(1),
                        "extension": { "name": "echo", "version": "0.1.0" },
                        "registrations": {
                            "tools": [{
                                "name": "say",
                                "description": "Echo a phrase back.",
                                "parameters": { "type": "object", "properties": { "phrase": { "type": "string" } }, "required": ["phrase"] }
                            }],
                            "subscriptions": ["turn_start", "turn_end", "message_end"]
                        }
                    }),
                )
            }
            "ping" => success(&id, json!({})),
            // `SEP_ECHO_BLOCK=1` turns the peer into a veto gate — used by the
            // host's tool_call-layering test.
            "hook" if std::env::var("SEP_ECHO_BLOCK").is_ok() => success(&id, json!({ "action": "block", "reason": "blocked by echo peer" })),
            "hook" => success(&id, json!({ "action": "continue" })),
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
            "tool/execute" => {
                let phrase = params.get("arguments").and_then(|a| a.get("phrase")).and_then(Value::as_str).unwrap_or("");
                success(&id, json!({ "content": phrase }))
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
