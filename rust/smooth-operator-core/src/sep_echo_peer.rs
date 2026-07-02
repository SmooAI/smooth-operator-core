//! `sep-echo-peer` — a minimal, dependency-free SEP extension in Rust.
//!
//! Reads JSON-RPC 2.0 frames as ndjson on stdin and replies on stdout. It is
//! the fixture-replay peer for the engine host's process-lifecycle tests (so
//! that suite needs no Node runtime in CI), and doubles as the smallest
//! possible reference SEP extension. Behavior mirrors the spec's `echo.mjs`:
//! handshake, answer `ping`, continue every `hook`, echo the `say` tool, and
//! exit on `shutdown`.

use std::io::{BufRead, Write};

use serde_json::{json, Value};

fn main() {
    let stdin = std::io::stdin();
    let stdout = std::io::stdout();
    let mut out = stdout.lock();

    for line in stdin.lock().lines() {
        let Ok(line) = line else { break };
        if line.trim().is_empty() {
            continue;
        }
        let Ok(msg): Result<Value, _> = serde_json::from_str(&line) else {
            continue;
        };

        // Notifications (no id) are observed and dropped.
        let Some(id) = msg.get("id").cloned() else { continue };
        let method = msg.get("method").and_then(Value::as_str).unwrap_or("");
        let params = msg.get("params").cloned().unwrap_or(Value::Null);

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
