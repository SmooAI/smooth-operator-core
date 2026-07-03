/**
 * SEP host — pure unit coverage of the security-critical policy and the parsing
 * seams, mirroring the Rust host's module-level tests (`fold_hook_chain`,
 * `effective_subscriptions`, `validate_command_context`, manifest discovery,
 * protocol frame classification). No subprocess is spawned here.
 */
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

import {
    codes,
    discover,
    effectiveSubscriptions,
    expandEnv,
    foldHookChain,
    type FoldedHook,
    type HookOutcome,
    type HookStep,
    hookDefaultTimeoutMs,
    hookFailClosed,
    hookTypeFromName,
    isNotification,
    isRequest,
    isResponse,
    type Message,
    parseHookOutcome,
    parseManifest,
    resolvedEnv,
    RpcError,
    validateCommandContext,
} from '../src/extension/index.js';

function writeExt(dir: string, name: string, body: string): void {
    const extDir = join(dir, name);
    mkdirSync(extDir, { recursive: true });
    writeFileSync(join(extDir, 'extension.toml'), body);
}

describe('foldHookChain — the security-critical hook policy', () => {
    const replied = (outcome: HookOutcome): HookStep => ({ kind: 'replied', outcome });
    const failed: HookStep = { kind: 'failed' };

    it('empty chain proceeds unchanged', () => {
        const input = { tool: 'rm' };
        expect(foldHookChain('tool_call', input, [])).toEqual<FoldedHook>({ kind: 'proceed', value: input });
    });

    it('continue keeps the value', () => {
        const steps: HookStep[] = [replied({ action: 'continue' }), replied({ action: 'continue' })];
        expect(foldHookChain('tool_result', { a: 1 }, steps)).toEqual<FoldedHook>({ kind: 'proceed', value: { a: 1 } });
    });

    it('modify threads the patch to the next extension', () => {
        const steps: HookStep[] = [replied({ action: 'modify', patch: { a: 2 } }), replied({ action: 'continue' })];
        expect(foldHookChain('context', { a: 1 }, steps)).toEqual<FoldedHook>({ kind: 'proceed', value: { a: 2 } });
    });

    it('block short-circuits and later modifies never apply', () => {
        const steps: HookStep[] = [replied({ action: 'block', reason: 'rm -rf blocked' }), replied({ action: 'modify', patch: { should: 'not apply' } })];
        expect(foldHookChain('tool_call', {}, steps)).toEqual<FoldedHook>({ kind: 'blocked', reason: 'rm -rf blocked' });
    });

    it('block without a reason gets a default', () => {
        const steps: HookStep[] = [replied({ action: 'block' })];
        expect(foldHookChain('user_bash', {}, steps)).toEqual<FoldedHook>({ kind: 'blocked', reason: 'blocked by user_bash hook' });
    });

    it('failure is fail-closed for tool_call and user_bash', () => {
        for (const hook of ['tool_call', 'user_bash'] as const) {
            const folded = foldHookChain(hook, {}, [failed]);
            expect(folded.kind).toBe('blocked');
            expect((folded as { reason: string }).reason).toContain('fail-closed');
        }
    });

    it('failure is fail-open for every other hook (value passes through)', () => {
        const steps: HookStep[] = [failed, replied({ action: 'continue' })];
        expect(foldHookChain('tool_result', { x: 9 }, steps)).toEqual<FoldedHook>({ kind: 'proceed', value: { x: 9 } });
    });

    it('modify then failure on a fail-open hook keeps the patch', () => {
        const steps: HookStep[] = [replied({ action: 'modify', patch: { x: 2 } }), failed];
        expect(foldHookChain('input', { x: 1 }, steps)).toEqual<FoldedHook>({ kind: 'proceed', value: { x: 2 } });
    });
});

describe('hook type policy + timeouts', () => {
    it('classifies fail-closed hooks and default timeouts', () => {
        expect(hookFailClosed('tool_call')).toBe(true);
        expect(hookFailClosed('user_bash')).toBe(true);
        expect(hookFailClosed('tool_result')).toBe(false);
        expect(hookFailClosed('message_end')).toBe(false);
        expect(hookDefaultTimeoutMs('tool_call')).toBe(60_000);
        expect(hookDefaultTimeoutMs('tool_result')).toBe(5_000);
        expect(hookTypeFromName('before_agent_start')).toBe('before_agent_start');
        expect(hookTypeFromName('nope')).toBeUndefined();
    });
});

describe('effectiveSubscriptions — clamp to declared', () => {
    it('no declared filter → handshake as-is', () => {
        expect(effectiveSubscriptions([], ['turn_start', 'turn_end'])).toEqual(new Set(['turn_start', 'turn_end']));
    });
    it('declared list clamps: an undeclared request is dropped', () => {
        expect(effectiveSubscriptions(['turn_start'], ['turn_start', 'tool_call'])).toEqual(new Set(['turn_start']));
    });
    it('declared but not requested → not subscribed', () => {
        expect(effectiveSubscriptions(['turn_start', 'turn_end'], ['turn_end'])).toEqual(new Set(['turn_end']));
    });
});

describe('validateCommandContext — the command-tier deadlock guard', () => {
    const ctx = (tier: string, token: string) => ({ context: { tier, token }, text: 'hi' });

    it('accepts a current command-tier context', () => {
        expect(() => validateCommandContext(ctx('command', 'epoch-4'), 4)).not.toThrow();
    });
    it('rejects an event-tier context', () => {
        expect(() => validateCommandContext(ctx('event', 'epoch-4'), 4)).toThrowError(RpcError);
        try {
            validateCommandContext(ctx('event', 'epoch-4'), 4);
        } catch (e) {
            expect((e as RpcError).code).toBe(codes.ContextViolation);
        }
    });
    it('rejects a stale epoch (reload bumped 4→5)', () => {
        try {
            validateCommandContext(ctx('command', 'epoch-4'), 5);
            throw new Error('should have thrown');
        } catch (e) {
            expect((e as RpcError).code).toBe(codes.ContextViolation);
        }
    });
    it('rejects missing and malformed tokens', () => {
        for (const bad of [{ text: 'hi' }, ctx('command', 'garbage'), ctx('command', 'epoch-')]) {
            try {
                validateCommandContext(bad, 1);
                throw new Error('should have thrown');
            } catch (e) {
                expect((e as RpcError).code).toBe(codes.ContextViolation);
            }
        }
    });
});

describe('parseHookOutcome', () => {
    it('parses each valid action', () => {
        expect(parseHookOutcome({ action: 'continue' })).toEqual({ action: 'continue' });
        expect(parseHookOutcome({ action: 'block', reason: 'no' })).toEqual({ action: 'block', reason: 'no' });
        expect(parseHookOutcome({ action: 'block' })).toEqual({ action: 'block' });
        expect(parseHookOutcome({ action: 'modify', patch: { a: 1 } })).toEqual({ action: 'modify', patch: { a: 1 } });
    });
    it('rejects an unknown action and a modify without patch', () => {
        expect(() => parseHookOutcome({ action: 'bogus' })).toThrowError(RpcError);
        expect(() => parseHookOutcome({ action: 'modify' })).toThrowError(RpcError);
        expect(() => parseHookOutcome(null)).toThrowError(RpcError);
    });
});

describe('protocol frame classification', () => {
    it('classifies request / notification / response', () => {
        const req: Message = { jsonrpc: '2.0', id: 1, method: 'ping', params: {} };
        expect(isRequest(req)).toBe(true);
        expect(isNotification(req)).toBe(false);
        const note: Message = { jsonrpc: '2.0', method: 'event', params: {} };
        expect(isNotification(note)).toBe(true);
        const ok: Message = { jsonrpc: '2.0', id: 2, result: {} };
        expect(isResponse(ok)).toBe(true);
        expect(isRequest(ok)).toBe(false);
    });
});

describe('manifest parse + discover + env expansion', () => {
    const MINIMAL = 'name = "echo"\nversion = "0.1.0"\n[run]\ncommand = "node"\nargs = ["echo.mjs"]\n';

    it('parses a minimal manifest with defaults', () => {
        const m = parseManifest(MINIMAL);
        expect(m.name).toBe('echo');
        expect(m.protocol).toBe(1);
        expect(m.run.command).toBe('node');
        expect(m.run.args).toEqual(['echo.mjs']);
        expect(m.disabled).toBe(false);
        expect(m.capabilities.events).toEqual([]);
    });

    it('parses a full manifest', () => {
        const m = parseManifest(
            'name = "gate"\nversion = "2.0.0"\nprotocol = 1\nhook_timeout_ms = 3000\n[run]\ncommand = "python3"\nargs = ["-m", "gate"]\nenv = { TOKEN = "${env:GATE_TOKEN}", STATIC = "x" }\n[capabilities]\nevents = ["turn_start", "tool_call"]\ntools = true\nui = true\n[resources]\nskills = "skills"\n',
        );
        expect(m.hookTimeoutMs).toBe(3000);
        expect(m.capabilities.tools && m.capabilities.ui && !m.capabilities.exec).toBe(true);
        expect(m.capabilities.events).toEqual(['turn_start', 'tool_call']);
        expect(m.resources.skills).toBe('skills');
    });

    it('rejects a manifest missing required fields', () => {
        expect(() => parseManifest('version = "1"\n[run]\ncommand = "c"\n')).toThrow();
        expect(() => parseManifest('not toml : : :')).toThrow();
    });

    it('resolvedEnv expands ${env:VAR}, unset → empty', () => {
        process.env.SEP_TEST_TOKEN = 'secret123';
        const m = parseManifest('name = "e"\nversion = "1"\n[run]\ncommand = "c"\nenv = { A = "pre-${env:SEP_TEST_TOKEN}-post", B = "${env:SEP_TEST_UNSET_XYZ}" }\n');
        const env = resolvedEnv(m);
        expect(env.A).toBe('pre-secret123-post');
        expect(env.B).toBe('');
        delete process.env.SEP_TEST_TOKEN;
    });

    it('expandEnv handles an unterminated reference', () => {
        expect(expandEnv('a${env:FOO')).toBe('a${env:FOO');
        expect(expandEnv('plain')).toBe('plain');
    });

    it('discover merges project over global', () => {
        const tmp = mkdtempSync(join(tmpdir(), 'sep-disc-'));
        const global = join(tmp, 'global');
        const project = join(tmp, 'project');
        writeExt(global, 'echo', 'name="echo"\nversion="1.0.0"\n[run]\ncommand="g"\n');
        writeExt(global, 'only_global', 'name="only_global"\nversion="1"\n[run]\ncommand="g"\n');
        writeExt(project, 'echo', 'name="echo"\nversion="2.0.0"\n[run]\ncommand="p"\n');
        writeExt(project, 'only_project', 'name="only_project"\nversion="1"\n[run]\ncommand="p"\n');

        const { extensions, failures } = discover(global, project);
        expect(failures).toEqual([]);
        expect(extensions.length).toBe(3);
        const echo = extensions.find((e) => e.manifest.name === 'echo')!;
        expect(echo.manifest.version).toBe('2.0.0');
        expect(echo.scope).toBe('project');
        expect(extensions.some((e) => e.manifest.name === 'only_global' && e.scope === 'global')).toBe(true);
    });

    it('discover tolerates one broken manifest', () => {
        const tmp = mkdtempSync(join(tmpdir(), 'sep-disc2-'));
        const global = join(tmp, 'g');
        writeExt(global, 'good', 'name="good"\nversion="1"\n[run]\ncommand="c"\n');
        writeExt(global, 'bad', 'this is not = = valid toml\n[[[');
        const { extensions, failures } = discover(global, undefined);
        expect(extensions.length).toBe(1);
        expect(extensions[0].manifest.name).toBe('good');
        expect(failures.length).toBe(1);
        expect(failures[0][0]).toContain('bad');
    });

    it('discover on missing dirs is empty, not an error', () => {
        const { extensions, failures } = discover('/no/such/global', '/no/such/project');
        expect(extensions).toEqual([]);
        expect(failures).toEqual([]);
    });
});
