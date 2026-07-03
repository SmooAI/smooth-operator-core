#!/usr/bin/env node
// Vendored copy of the SEP conformance echo peer (spec/extension/conformance/echo.mjs
// in the smooth-operator repo). A dependency-free extension subprocess that speaks
// JSON-RPC 2.0 ndjson over stdin/stdout, so the TS engine host can spawn it and
// replay conformance/fixtures.json against a live process. Kept in sync with the
// canonical spec copy; only node:readline / node:process are used so it runs anywhere.
//
// Test knobs (env vars) mirror the Rust `sep-echo-peer`:
//   SEP_ECHO_BLOCK=1  → every hook replies { action: 'block' }
//   SEP_ECHO_PATCH=1  → tool_result hooks reply { action: 'modify', patch: {...} }
//   SEP_ECHO_HANG=1   → hooks never reply (exercise host hook timeouts)

import { createInterface } from 'node:readline';
import process from 'node:process';

const rl = createInterface({ input: process.stdin, terminal: false });
const BLOCK = process.env.SEP_ECHO_BLOCK === '1';
const PATCH = process.env.SEP_ECHO_PATCH === '1';
const HANG = process.env.SEP_ECHO_HANG === '1';

function reply(id, result) {
    process.stdout.write(`${JSON.stringify({ jsonrpc: '2.0', id, result })}\n`);
}

function replyError(id, code, message) {
    process.stdout.write(`${JSON.stringify({ jsonrpc: '2.0', id, error: { code, message } })}\n`);
}

rl.on('line', (line) => {
    if (!line.trim()) return;
    const frame = JSON.parse(line);
    const { id, method, params } = frame;
    const isNotification = id === undefined;

    switch (method) {
        case 'initialize':
            reply(id, {
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
                    subscriptions: ['turn_start', 'turn_end', 'message_end'],
                },
            });
            break;

        case 'ping':
            reply(id, {});
            break;

        case 'hook':
            if (HANG) break; // never reply → host times out
            if (BLOCK) {
                reply(id, { action: 'block', reason: 'blocked by echo' });
            } else if (PATCH && params?.hook === 'tool_result') {
                reply(id, { action: 'modify', patch: { content: '[patched by echo]', is_error: false } });
            } else {
                reply(id, { action: 'continue' });
            }
            break;

        case 'tool/execute':
            reply(id, { content: params?.arguments?.phrase ?? '' });
            break;

        case 'command/execute':
            reply(id, { content: `ran ${params?.command ?? ''}` });
            break;

        case 'command/complete':
            reply(id, { completions: [{ value: `${params?.partial ?? ''}-done`, description: 'echo completion' }] });
            break;

        case 'shutdown':
            reply(id, {});
            process.exit(0);
            break;

        case 'event':
        case '$/cancel':
            // Fire-and-forget notifications this demo extension doesn't act on.
            break;

        default:
            if (!isNotification) {
                replyError(id, -32601, `method not found: ${method}`);
            }
            break;
    }
});
