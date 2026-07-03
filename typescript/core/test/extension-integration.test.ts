/**
 * SEP host — live integration coverage. Spawns the dependency-free echo peer
 * (`node test/sep/echo.mjs`) through the real {@link ExtensionHost} and asserts the
 * headline claims the Rust `tests/sep_agent_integration.rs` + server
 * `tests/sep_extension_host.rs` make, adapted to the TS engine:
 *
 *  - an extension's `say` tool surfaces as a dotted `echo.say` proxy and executes
 *    end-to-end when a (mock) LLM calls it through a real {@link SmoothAgent} turn;
 *  - the same array filter a server uses for `enabled_tools` drops `echo.say`
 *    exactly like a native tool (SMOODEV-590 parity);
 *  - a `tool_call` hook can veto (block), fail CLOSED on timeout, and a
 *    `tool_result` hook can patch a completed result (fail-open).
 */
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, describe, expect, it } from 'vitest';

import { SmoothAgent, type Tool } from '../src/index.js';
import { MockLlmProvider } from '../src/llmProvider.js';
import { DefaultHostDelegate, discover, ExtensionHost, type HostInfo, type WorkspaceInfo } from '../src/extension/index.js';

const __dirname = dirname(fileURLToPath(import.meta.url));
const ECHO_PEER = join(__dirname, 'sep', 'echo.mjs');

const HOST: HostInfo = { name: 'test-host', version: '0.0.0' };
const TRUSTED: WorkspaceInfo = { root: '/ws', trusted: true };

const dirs: string[] = [];
afterEach(() => {
    for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
});

/** Write `<tmp>/echo/extension.toml` running `node echo.mjs`, with optional peer env + hook timeout. */
function writeEchoManifest(env: Record<string, string> = {}, hookTimeoutMs?: number): string {
    const tmp = mkdtempSync(join(tmpdir(), 'sep-int-'));
    dirs.push(tmp);
    const extDir = join(tmp, 'echo');
    mkdirSync(extDir, { recursive: true });
    const envLine = Object.keys(env).length
        ? `env = { ${Object.entries(env)
              .map(([k, v]) => `${k} = "${v}"`)
              .join(', ')} }\n`
        : '';
    const timeoutLine = hookTimeoutMs ? `hook_timeout_ms = ${hookTimeoutMs}\n` : '';
    const toml = `name = "echo"\nversion = "0.1.0"\n${timeoutLine}[run]\ncommand = "node"\nargs = ["${ECHO_PEER}"]\n${envLine}[capabilities]\ntools = true\nevents = ["turn_start", "turn_end", "message_end"]\n`;
    writeFileSync(join(extDir, 'extension.toml'), toml);
    return tmp;
}

async function loadHost(tmp: string, env: Record<string, string> = {}): Promise<ExtensionHost> {
    // env is baked into the manifest, not passed here — kept for signature parity.
    void env;
    const { extensions, failures } = discover(tmp, undefined);
    expect(failures).toEqual([]);
    const { host, failures: loadFailures } = await ExtensionHost.load(extensions, HOST, TRUSTED, 'headless', [], new DefaultHostDelegate());
    expect(loadFailures).toEqual([]);
    return host;
}

describe('ExtensionHost — live echo peer', () => {
    it('exposes say as echo.say and executes it end-to-end through a real agent turn', async () => {
        const host = await loadHost(writeEchoManifest());
        try {
            expect(host.names()).toEqual(['echo']);
            const tools = host.tools();
            expect(tools.map((t) => t.name)).toContain('echo.say');

            const mock = new MockLlmProvider();
            mock.pushToolCall('c1', 'echo.say', JSON.stringify({ phrase: 'hello from the LLM' }));
            mock.pushText('done');

            const agent = new SmoothAgent(mock, { instructions: 'system', tools, maxIterations: 4 });
            const result = await agent.run('go');

            expect(result.text).toBe('done');
            expect(result.toolCalls).toBe(1);
            // The say tool echoed the phrase back — assert via a direct execute too.
            const say = tools.find((t) => t.name === 'echo.say')!;
            expect(await say.execute({ phrase: 'hi there' })).toBe('hi there');
        } finally {
            await host.shutdownAll();
        }
    });

    it('extension tools honor an enabled_tools array filter (SMOODEV-590 parity)', async () => {
        const host = await loadHost(writeEchoManifest());
        try {
            const all: Tool[] = host.tools();
            expect(all.some((t) => t.name === 'echo.say')).toBe(true);
            // A server's per-agent allow-list is a plain filter over the tool array.
            const keep = (enabled: string[]) => all.filter((t) => enabled.includes(t.name));
            expect(keep(['echo.say']).some((t) => t.name === 'echo.say')).toBe(true);
            expect(keep(['some_builtin']).some((t) => t.name === 'echo.say')).toBe(false);
        } finally {
            await host.shutdownAll();
        }
    });

    it('subscribes only to declared events and fans out without throwing', async () => {
        const host = await loadHost(writeEchoManifest());
        try {
            expect(host.hasSubscriber('turn_start')).toBe(true);
            expect(host.hasSubscriber('never_declared')).toBe(false);
            host.dispatchEvent('turn_start', { agent_id: 'a1' }); // must not throw
        } finally {
            await host.shutdownAll();
        }
    });

    it('a tool_call hook vetoes the call (block)', async () => {
        const host = await loadHost(writeEchoManifest({ SEP_ECHO_BLOCK: '1' }));
        try {
            const folded = await host.runToolCallHook('danger', {});
            expect(folded.kind).toBe('blocked');
            expect((folded as { reason: string }).reason).toContain('blocked by echo');
        } finally {
            await host.shutdownAll();
        }
    });

    it('a hung tool_call hook fails CLOSED without stalling', async () => {
        const host = await loadHost(writeEchoManifest({ SEP_ECHO_HANG: '1' }, 200));
        try {
            const start = Date.now();
            const folded = await host.runToolCallHook('danger', {});
            expect(Date.now() - start).toBeLessThan(5_000);
            expect(folded.kind).toBe('blocked');
            expect((folded as { reason: string }).reason).toContain('fail-closed');
        } finally {
            await host.shutdownAll();
        }
    });

    it('a tool_result hook patches the completed result (fail-open)', async () => {
        const host = await loadHost(writeEchoManifest({ SEP_ECHO_PATCH: '1' }));
        try {
            const folded = await host.runHook('tool_result', { tool: 'bash', content: 'total 0\n', is_error: false });
            expect(folded.kind).toBe('proceed');
            expect((folded as { value: { content: string } }).value.content).toBe('[patched by echo]');
        } finally {
            await host.shutdownAll();
        }
    });

    it('skips project-scoped extensions in an untrusted workspace', async () => {
        const tmp = mkdtempSync(join(tmpdir(), 'sep-proj-'));
        dirs.push(tmp);
        const extRoot = join(tmp, '.smooth', 'extensions');
        const extDir = join(extRoot, 'echo');
        mkdirSync(extDir, { recursive: true });
        writeFileSync(join(extDir, 'extension.toml'), `name = "echo"\nversion = "0.1.0"\n[run]\ncommand = "node"\nargs = ["${ECHO_PEER}"]\n[capabilities]\ntools = true\n`);
        const { extensions } = discover(undefined, extRoot);
        expect(extensions.length).toBe(1);
        const { host } = await ExtensionHost.load(extensions, HOST, { root: tmp, trusted: false }, 'headless', [], new DefaultHostDelegate());
        expect(host.isEmpty()).toBe(true);
        await host.shutdownAll();
    });
});
