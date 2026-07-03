#!/usr/bin/env python3
"""The SEP fixture-replay peer, in Python — a dependency-free demo extension used as
the conformance target for the Python host's live-process tests.

Speaks JSON-RPC 2.0 ndjson over stdin/stdout, exactly as a real extension would, so
the host can spawn it and drive a real handshake / tool call / hook / shutdown
against a live subprocess. Byte-for-byte the behaviour of ``spec/extension/
conformance/echo.mjs`` (kept in Python here so the core's test lane needs no node).
"""

import json
import sys


def reply(rid, result):
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": rid, "result": result}) + "\n")
    sys.stdout.flush()


def reply_error(rid, code, message):
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": rid, "error": {"code": code, "message": message}}) + "\n")
    sys.stdout.flush()


def main() -> None:
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        frame = json.loads(line)
        rid = frame.get("id")
        method = frame.get("method")
        params = frame.get("params") or {}
        is_notification = rid is None

        if method == "initialize":
            reply(
                rid,
                {
                    "protocol_version": min(params.get("protocol_version", 1), 1),
                    "extension": {"name": "echo", "version": "0.1.0"},
                    "registrations": {
                        "tools": [
                            {
                                "name": "say",
                                "description": "Echo a phrase back.",
                                "parameters": {
                                    "type": "object",
                                    "properties": {"phrase": {"type": "string"}},
                                    "required": ["phrase"],
                                },
                            }
                        ],
                        "commands": [{"name": "echo-cmd", "description": "Echo a slash-command back."}],
                        "subscriptions": ["turn_start", "turn_end", "message_end"],
                    },
                },
            )
        elif method == "ping":
            reply(rid, {})
        elif method == "hook":
            reply(rid, {"action": "continue"})
        elif method == "tool/execute":
            phrase = (params.get("arguments") or {}).get("phrase", "")
            reply(rid, {"content": phrase, "is_error": False})
        elif method == "command/execute":
            reply(rid, {"content": f"ran {params.get('command', '')}"})
        elif method == "shutdown":
            reply(rid, {})
            sys.exit(0)
        elif method in ("event", "$/cancel"):
            pass  # fire-and-forget notifications this demo doesn't act on
        else:
            if not is_notification:
                reply_error(rid, -32601, f"method not found: {method}")


if __name__ == "__main__":
    main()
