#!/usr/bin/env node
// sep-echo-peer — a dependency-free SEP extension peer for the C# host's live tests.
// Reads JSON-RPC 2.0 ndjson on stdin, replies on stdout. Behavior mirrors the Rust
// `sep-echo-peer` + the spec's echo.mjs: handshake, ping, continue every hook, echo the
// `say` tool, run a command, complete a command, exit on shutdown.
//
// Env-gated test modes (match the Rust peer):
// - SEP_ECHO_BLOCK=1 — every hook vetoes (the tool_call-layering test).
// - SEP_ECHO_PATCH=1 — the tool_result hook rewrites content via a Modify outcome.
// - SEP_ECHO_HANG=1  — every hook hangs forever (drives the fail-closed timeout path).
// - SEP_ECHO_UI=1    — tool/execute sends a ui/request(confirm) echoing the negotiated
//                      ui_capabilities, and returns the host's answer as the tool content.

import { createInterface } from 'node:readline';
import process from 'node:process';

const rl = createInterface({ input: process.stdin, terminal: false });

const BLOCK = !!process.env.SEP_ECHO_BLOCK;
const PATCH = !!process.env.SEP_ECHO_PATCH;
const HANG = !!process.env.SEP_ECHO_HANG;
const UI = !!process.env.SEP_ECHO_UI;

let uiCaps = [];
// When in UI mode, a tool/execute parks awaiting the host's reply to this id.
let pendingUi = null;

function write(frame) {
    process.stdout.write(`${JSON.stringify(frame)}\n`);
}
function success(id, result) {
    write({ jsonrpc: '2.0', id, result });
}
function error(id, code, message) {
    write({ jsonrpc: '2.0', id, error: { code, message } });
}

rl.on('line', (line) => {
    if (!line.trim()) return;
    let msg;
    try {
        msg = JSON.parse(line);
    } catch {
        return;
    }
    const { id, method, params } = msg;

    // A reply to our in-flight ui/request resolves the parked tool/execute.
    if (pendingUi && id !== undefined && method === undefined && id === pendingUi.reqId) {
        const resolve = pendingUi.resolve;
        pendingUi = null;
        resolve(msg);
        return;
    }

    if (id === undefined) {
        // Notification — nothing to act on here ($/cancel, event).
        return;
    }

    switch (method) {
        case 'initialize':
            if (Array.isArray(params?.ui_capabilities)) uiCaps = params.ui_capabilities;
            success(id, {
                protocol_version: Math.min(params?.protocol_version ?? 1, 1),
                extension: { name: 'echo', version: '0.1.0' },
                registrations: {
                    tools: [
                        {
                            name: 'say',
                            description: 'Echo a phrase back.',
                            parameters: { type: 'object', properties: { phrase: { type: 'string' } }, required: ['phrase'] },
                        },
                    ],
                    commands: [{ name: 'echo-cmd', description: 'Echo a slash-command back.' }],
                    shortcuts: [{ key: 'ctrl+e', command: 'echo-cmd', description: 'Run echo-cmd' }],
                    subscriptions: ['turn_start', 'turn_end', 'message_end', 'session_start', 'session_shutdown'],
                },
            });
            break;

        case 'ping':
            success(id, {});
            break;

        case 'hook':
            if (HANG) break; // never reply
            if (BLOCK) {
                success(id, { action: 'block', reason: 'blocked by echo peer' });
            } else if (PATCH && params?.hook === 'tool_result') {
                success(id, { action: 'modify', patch: { content: '[patched by echo]' } });
            } else {
                success(id, { action: 'continue' });
            }
            break;

        case 'tool/execute':
            if (UI) {
                const reqId = 9001;
                pendingUi = {
                    reqId,
                    resolve: (reply) => {
                        let confirmed = 'no-reply';
                        if (reply.error) {
                            confirmed = `error:${reply.error.code}`;
                        } else if (reply.result && typeof reply.result.confirmed === 'boolean') {
                            confirmed = String(reply.result.confirmed);
                        } else if (reply.result && reply.result.cancelled) {
                            confirmed = 'cancelled';
                        }
                        success(id, { content: `confirmed=${confirmed}` });
                    },
                };
                write({ jsonrpc: '2.0', id: reqId, method: 'ui/request', params: { kind: 'confirm', prompt: `caps=${uiCaps.join(',')}` } });
            } else {
                success(id, { content: params?.arguments?.phrase ?? '' });
            }
            break;

        case 'command/execute':
            success(id, { content: `ran ${params?.command ?? ''}` });
            break;

        case 'command/complete':
            success(id, { completions: [{ value: `${params?.partial ?? ''}-done`, description: 'echo completion' }] });
            break;

        case 'shutdown':
            success(id, {});
            process.exit(0);
            break;

        default:
            error(id, -32601, `method not found: ${method}`);
            break;
    }
});
