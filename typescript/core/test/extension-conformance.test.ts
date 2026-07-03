/**
 * SEP conformance replay — the TS host's side of the shared fixture suite.
 *
 * Two layers, mirroring how the Rust host proves conformance:
 *  1. STATIC — the vendored `spec/extension/conformance/fixtures.json` (a copy of
 *     the canonical suite in the smooth-operator repo) is well-formed and its
 *     method fixtures parse/classify through the host's typed view of the wire.
 *  2. LIVE — spawn the dependency-free echo peer (`node test/sep/echo.mjs`) through
 *     the real {@link ExtensionProcess}, handshake, and replay each request fixture,
 *     asserting the live process answers each method with the expected reply. This
 *     is the host-side analog of the SDK's `runConformance` (client side).
 */
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterAll, describe, expect, it } from 'vitest';

import {
    type CommandCompleteResult,
    type CommandExecuteResult,
    DefaultInboundHandler,
    type EventParams,
    ExtensionProcess,
    type InitializeResult,
    isNotification,
    isRequest,
    isResponse,
    type Message,
    parseHookOutcome,
    PROTOCOL_VERSION,
    type ToolExecuteResult,
} from '../src/extension/index.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const SEP_DIR = join(__dirname, 'sep');
const ECHO_PEER = join(SEP_DIR, 'echo.mjs');

interface Fixture {
    $schema_ref: string;
    instance: unknown;
}
const rawFixtures = JSON.parse(readFileSync(join(SEP_DIR, 'fixtures.json'), 'utf8')) as Record<string, unknown>;
const fixtures = rawFixtures as Record<string, Fixture>;
const invalidList = rawFixtures.$invalid as Array<{ name: string; instance: unknown }>;

function instance(name: string): unknown {
    const f = fixtures[name];
    if (!f?.instance) throw new Error(`fixture \`${name}\` missing or has no instance`);
    return f.instance;
}
function invalid(name: string): unknown {
    const f = invalidList.find((e) => e.name === name);
    if (!f) throw new Error(`invalid fixture \`${name}\` missing`);
    return f.instance;
}

describe('SEP fixtures — static well-formedness + typed parse', () => {
    it('every non-$ fixture carries $schema_ref + instance (full set)', () => {
        let count = 0;
        for (const [name, f] of Object.entries(fixtures)) {
            if (name.startsWith('$')) continue;
            const fx = f as Fixture;
            expect(typeof fx.$schema_ref).toBe('string');
            expect(fx.instance).toBeDefined();
            count++;
        }
        expect(count).toBeGreaterThanOrEqual(40);
    });

    it('frame fixtures classify correctly', () => {
        expect(isRequest(instance('frame_request') as Message)).toBe(true);
        expect(isNotification(instance('frame_notification') as Message)).toBe(true);
        const ok = instance('frame_success_response') as Message;
        expect(isResponse(ok)).toBe(true);
        expect(ok.result).toBeDefined();
        for (const n of ['frame_error_response', 'error_blocked', 'error_cancelled', 'error_context_violation']) {
            expect((instance(n) as Message).error).toBeDefined();
        }
    });

    it('hook outcome fixtures parse; the $invalid ones are rejected', () => {
        expect(parseHookOutcome(instance('hook_outcome_continue'))).toEqual({ action: 'continue' });
        expect(parseHookOutcome(instance('hook_outcome_block'))).toMatchObject({ action: 'block' });
        expect(parseHookOutcome(instance('hook_outcome_modify'))).toMatchObject({ action: 'modify' });
        expect(() => parseHookOutcome(invalid('hook_outcome_bogus_action'))).toThrow();
        expect(() => parseHookOutcome(invalid('hook_outcome_modify_missing_patch'))).toThrow();
    });

    it('a dispatched event is seq-numbered; the events_lost marker is not', () => {
        const normal = instance('event_params') as EventParams;
        expect(normal.seq).toBeTypeOf('number');
        const lost = instance('event_events_lost') as EventParams;
        expect(lost.event).toBe('events_lost');
        expect(lost.seq).toBeUndefined();
        expect((lost.payload as { lost: number }).lost).toBe(12);
    });
});

describe('SEP conformance — live echo peer replay', () => {
    const proc = ExtensionProcess.spawn({ command: 'node', args: [ECHO_PEER], env: {} }, new DefaultInboundHandler());
    afterAll(async () => {
        await proc.shutdown(1000);
    });

    it('initialize returns negotiated version + registrations', async () => {
        const base = instance('initialize_params') as Record<string, unknown>;
        const result = (await proc.request('initialize', { ...base, protocol_version: PROTOCOL_VERSION }, 10_000)) as InitializeResult;
        expect(result.protocol_version).toBe(1);
        expect(result.extension.name).toBe('echo');
        expect(result.registrations?.tools?.some((t) => t.name === 'say')).toBe(true);
    });

    it('ping answers empty', async () => {
        expect(await proc.request('ping', {}, 5_000)).toEqual({});
    });

    it('tool/execute echoes the phrase', async () => {
        const result = (await proc.request('tool/execute', instance('tool_execute_params'), 5_000)) as ToolExecuteResult;
        expect(result.content).toBe('hello');
    });

    it('command/execute runs the command', async () => {
        const result = (await proc.request('command/execute', instance('command_execute_params'), 5_000)) as CommandExecuteResult;
        expect(result.content).toBe('ran say');
    });

    it('command/complete returns completions', async () => {
        const result = (await proc.request('command/complete', instance('command_complete_params'), 5_000)) as CommandCompleteResult;
        expect(result.completions.length).toBeGreaterThan(0);
        expect(result.completions[0].value).toBe('on-done');
    });

    it('an unknown method returns MethodNotFound', async () => {
        await expect(proc.request('bogus/method', {}, 5_000)).rejects.toThrow(/method not found/);
    });
});
