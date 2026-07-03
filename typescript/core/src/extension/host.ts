/**
 * `ExtensionHost` — orchestrates the loaded extensions: hook chaining in load
 * order, non-blocking event fanout, tool proxies, and the ext→host delegate seam.
 * The TS sibling of the Rust host's `extension/host.rs`.
 *
 * The security-critical part is {@link foldHookChain}: how per-extension hook
 * outcomes combine, and what happens on timeout/crash. It is a pure function so it
 * can be tested exhaustively against adversarial inputs without spawning anything.
 */

import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import type { Tool } from '../agent.js';
import { defaultGlobalDir, type DiscoveredExtension, resolvedEnv } from './manifest.js';
import { ExtensionProcess, type InboundHandler, type SpawnSpec } from './process.js';
import {
    codes,
    type CommandCompleteResult,
    type CommandExecuteResult,
    type CommandRegistration,
    type Completion,
    type Context,
    type HookOutcome,
    type HostInfo,
    type InitializeParams,
    type InitializeResult,
    method,
    parseHookOutcome,
    PROTOCOL_VERSION,
    RpcError,
    type ShortcutRegistration,
    type Tier,
    type WorkspaceInfo,
} from './protocol.js';
import { ExtensionTool } from './tool_proxy.js';

export { PROTOCOL_VERSION };

/** Classifies a hook by its failure policy and default timeout. */
export type HookType =
    | 'tool_call'
    | 'user_bash'
    | 'tool_result'
    | 'input'
    | 'before_agent_start'
    | 'context'
    | 'before_provider_request'
    | 'message_end'
    | 'session_before_compact'
    | 'session_before_tree';

const HOOK_TYPES = new Set<string>([
    'tool_call',
    'user_bash',
    'tool_result',
    'input',
    'before_agent_start',
    'context',
    'before_provider_request',
    'message_end',
    'session_before_compact',
    'session_before_tree',
]);

/** The two fail-closed hooks: they gate execution, so a timeout/crash BLOCKS. */
const FAIL_CLOSED_HOOKS = new Set<HookType>(['tool_call', 'user_bash']);

export function hookTypeFromName(name: string): HookType | undefined {
    return HOOK_TYPES.has(name) ? (name as HookType) : undefined;
}

/**
 * Fail-closed hooks (`tool_call`, `user_bash`) block the operation when an
 * extension times out or crashes. Everything else fails open (proceeds).
 */
export function hookFailClosed(hook: HookType): boolean {
    return FAIL_CLOSED_HOOKS.has(hook);
}

/**
 * Default hook timeout (ms): 60s for fail-closed (they gate execution), 5s for
 * fail-open. Manifest `hook_timeout_ms` overrides this.
 */
export function hookDefaultTimeoutMs(hook: HookType): number {
    return hookFailClosed(hook) ? 60_000 : 5_000;
}

/** One extension's reply within a hook chain, as seen by the fold. */
export type HookStep = { kind: 'replied'; outcome: HookOutcome } | { kind: 'failed' };

/** The folded result of a whole hook chain. */
export type FoldedHook = { kind: 'proceed'; value: unknown } | { kind: 'blocked'; reason: string };

/**
 * Fold a hook chain over `input`, in load order. `steps` are the per-extension
 * results in that order. This is the security-critical policy:
 *
 * - `continue` → value unchanged, next extension sees it.
 * - `modify` → value replaced by the patch, next extension sees the patch.
 * - `block` → short-circuit; the operation is vetoed (honored for every hook).
 * - failed → for a fail-closed hook, block; for a fail-open hook, proceed unchanged.
 */
export function foldHookChain(hook: HookType, input: unknown, steps: readonly HookStep[]): FoldedHook {
    let current = input;
    for (const step of steps) {
        if (step.kind === 'failed') {
            if (hookFailClosed(hook)) return { kind: 'blocked', reason: `${hook} hook failed (fail-closed)` };
            // fail-open: proceed with the current value.
            continue;
        }
        const outcome = step.outcome;
        if (outcome.action === 'continue') continue;
        if (outcome.action === 'modify') {
            current = outcome.patch;
            continue;
        }
        // block
        return { kind: 'blocked', reason: outcome.reason ?? `blocked by ${hook} hook` };
    }
    return { kind: 'proceed', value: current };
}

/**
 * Effective event subscriptions: what the extension asked for at handshake,
 * clamped to what its manifest `[capabilities] events` declared. An empty declared
 * list means "no declared filter" → trust the handshake as-is; a non-empty list is
 * the outer bound the extension can never widen past.
 */
export function effectiveSubscriptions(declared: readonly string[], requested: readonly string[]): Set<string> {
    if (declared.length === 0) return new Set(requested);
    const allow = new Set(declared);
    return new Set(requested.filter((s) => allow.has(s)));
}

/** Parse the epoch embedded in a context token minted by {@link ExtensionHost.context} (`epoch-<N>`). */
function tokenEpoch(token: string): number | undefined {
    if (!token.startsWith('epoch-')) return undefined;
    const n = token.slice('epoch-'.length);
    if (n.length === 0 || !/^\d+$/.test(n)) return undefined;
    return Number(n);
}

/**
 * The two-tier deadlock guard: a session-mutating ext→host action is valid only
 * when it presents a COMMAND-tier context whose epoch is still current. An
 * event-tier context, or a stale token minted before a reload bumped the epoch, is
 * rejected with `-32003 ContextViolation`. Security-critical; a pure function so it
 * can be tested exhaustively.
 */
export function validateCommandContext(params: unknown, currentEpoch: number): void {
    const ctx = (params as { context?: { tier?: unknown; token?: unknown } } | undefined)?.context;
    if (ctx?.tier !== 'command') throw new RpcError(codes.ContextViolation, 'session action requires a command-tier context');
    const token = typeof ctx.token === 'string' ? ctx.token : '';
    const epoch = tokenEpoch(token);
    if (epoch === undefined || epoch !== currentEpoch) {
        throw new RpcError(codes.ContextViolation, 'session action presented a stale context (epoch mismatch)');
    }
}

// ---------------------------------------------------------------------------
// Host delegate: the ext→host seam (ui / kv / exec / session / trust).
// ---------------------------------------------------------------------------

/**
 * The host's side of ext→host requests. The engine ships headless defaults
 * ({@link DefaultHostDelegate}); frontends (smooth-code, the daemon, the servers)
 * subclass it and override.
 */
export interface HostDelegate {
    /** Answer a `ui/request`. Headless default: no UI available. */
    uiRequest(ext: string, params: unknown): Promise<unknown>;
    /** `kv/get`. */
    kvGet(ext: string, key: string): Promise<unknown>;
    /** `kv/set`. */
    kvSet(ext: string, key: string, value: unknown): Promise<void>;
    /** `exec/run`. Headless default: deny. */
    execRun(ext: string, params: unknown): Promise<unknown>;
    /** `session/send_message`. Context already validated. Default: unavailable. */
    sessionSendMessage(ext: string, params: unknown): Promise<unknown>;
    /** `session/send_user_message`. Context already validated. Default: unavailable. */
    sessionSendUserMessage(ext: string, params: unknown): Promise<unknown>;
    /** `session/append_entry`. Context already validated. Default: unavailable. */
    sessionAppendEntry(ext: string, params: unknown): Promise<unknown>;
    /** A `tool/update` progress notification during an in-flight `tool/execute`. Fire-and-forget. */
    toolUpdate(ext: string, params: unknown): void;
}

/** Per-extension kv state file: `<globalDir>/<name>/state.json`. */
function kvFilePath(ext: string): string | undefined {
    const dir = defaultGlobalDir();
    return dir ? join(dir, ext, 'state.json') : undefined;
}

function kvFileLoad(ext: string): Record<string, unknown> {
    const path = kvFilePath(ext);
    if (!path || !existsSync(path)) return {};
    try {
        return JSON.parse(readFileSync(path, 'utf8')) as Record<string, unknown>;
    } catch {
        return {};
    }
}

function kvFileStore(ext: string, map: Record<string, unknown>): void {
    const path = kvFilePath(ext);
    if (!path) throw new RpcError(codes.InternalError, 'no home dir for kv store');
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, JSON.stringify(map, null, 2));
}

/** The engine's headless delegate: NoUI, JSON-file kv, exec denied, session disabled. */
export class DefaultHostDelegate implements HostDelegate {
    async uiRequest(_ext: string, _params: unknown): Promise<unknown> {
        throw new RpcError(codes.NoUI, 'no UI available (headless host)');
    }
    async kvGet(ext: string, key: string): Promise<unknown> {
        return kvFileLoad(ext)[key] ?? null;
    }
    async kvSet(ext: string, key: string, value: unknown): Promise<void> {
        const map = kvFileLoad(ext);
        map[key] = value;
        kvFileStore(ext, map);
    }
    async execRun(_ext: string, _params: unknown): Promise<unknown> {
        throw new RpcError(codes.NotTrusted, 'exec/run is not permitted on the headless host');
    }
    async sessionSendMessage(_ext: string, _params: unknown): Promise<unknown> {
        throw new RpcError(codes.CapabilityDisabled, 'session actions are unavailable on this host');
    }
    async sessionSendUserMessage(_ext: string, _params: unknown): Promise<unknown> {
        throw new RpcError(codes.CapabilityDisabled, 'session actions are unavailable on this host');
    }
    async sessionAppendEntry(_ext: string, _params: unknown): Promise<unknown> {
        throw new RpcError(codes.CapabilityDisabled, 'session actions are unavailable on this host');
    }
    toolUpdate(_ext: string, _params: unknown): void {
        // Headless: progress is dropped. A frontend/daemon overrides this.
    }
}

/**
 * Bridges the process reader's ext→host requests to the {@link HostDelegate}.
 * Holds a getter for the host's live epoch so it can reject stale/event-tier
 * session actions.
 */
class HostInbound implements InboundHandler {
    constructor(
        private readonly ext: string,
        private readonly delegate: HostDelegate,
        private readonly currentEpoch: () => number,
    ) {}

    async handleRequest(methodName: string, params: unknown): Promise<unknown> {
        switch (methodName) {
            case method.PING:
                return {};
            case method.UI_REQUEST:
                return this.delegate.uiRequest(this.ext, params);
            case method.EXEC_RUN:
                return this.delegate.execRun(this.ext, params);
            // Session actions are the tier-guarded set: validate the presented
            // context (command tier + current epoch) BEFORE touching the delegate.
            case method.SESSION_SEND_MESSAGE:
                validateCommandContext(params, this.currentEpoch());
                return this.delegate.sessionSendMessage(this.ext, params);
            case method.SESSION_SEND_USER_MESSAGE:
                validateCommandContext(params, this.currentEpoch());
                return this.delegate.sessionSendUserMessage(this.ext, params);
            case method.SESSION_APPEND_ENTRY:
                validateCommandContext(params, this.currentEpoch());
                return this.delegate.sessionAppendEntry(this.ext, params);
            case 'kv/get': {
                const key = (params as { key?: unknown })?.key;
                return { value: await this.delegate.kvGet(this.ext, typeof key === 'string' ? key : '') };
            }
            case 'kv/set': {
                const p = params as { key?: unknown; value?: unknown };
                await this.delegate.kvSet(this.ext, typeof p?.key === 'string' ? p.key : '', p?.value ?? null);
                return {};
            }
            default:
                throw new RpcError(codes.MethodNotFound, `method not found: ${methodName}`);
        }
    }

    handleNotification(methodName: string, params: unknown): void {
        if (methodName === method.TOOL_UPDATE) this.delegate.toolUpdate(this.ext, params);
    }
}

// ---------------------------------------------------------------------------
// ExtensionHost
// ---------------------------------------------------------------------------

/** A loaded, initialized extension. */
interface Loaded {
    name: string;
    process: ExtensionProcess;
    init: InitializeResult;
    subscriptions: Set<string>;
    /** The manifest's declared event allow-list — the clamp `subscriptions` can't widen past. */
    declaredEvents: string[];
    hookTimeoutMs?: number;
}

/** A `(name, error message)` pair for an extension that failed to load. */
export type LoadFailure = [name: string, error: string];

/** Orchestrates the set of loaded extensions in load order. */
export class ExtensionHost {
    private extensions: Loaded[] = [];
    private epoch = 1;

    private constructor(
        private readonly host: HostInfo,
        private readonly workspace: WorkspaceInfo,
        private readonly mode: string,
        private readonly uiCapabilities: string[],
    ) {}

    /** An empty host: no extensions, every hook a passthrough. The zero-cost default. */
    static empty(): ExtensionHost {
        return new ExtensionHost(
            { name: 'smooth-operator-core', version: '0.0.0' },
            { root: '', trusted: false },
            'headless',
            [],
        );
    }

    /**
     * Load and initialize each discovered extension. Per-extension failures (spawn,
     * handshake) are tolerated and returned alongside the host. In an untrusted
     * workspace, project-scoped extensions are skipped.
     */
    static async load(
        discovered: DiscoveredExtension[],
        host: HostInfo,
        workspace: WorkspaceInfo,
        mode: string,
        uiCapabilities: string[],
        delegate: HostDelegate,
    ): Promise<{ host: ExtensionHost; failures: LoadFailure[] }> {
        const self = new ExtensionHost(host, workspace, mode, uiCapabilities);
        const failures: LoadFailure[] = [];
        for (const ext of discovered) {
            const name = ext.manifest.name;
            if (ext.manifest.disabled) continue;
            if (ext.scope === 'project' && !workspace.trusted) continue;
            try {
                self.extensions.push(await self.loadOne(ext, delegate));
            } catch (e) {
                failures.push([name, e instanceof Error ? e.message : String(e)]);
            }
        }
        return { host: self, failures };
    }

    private async loadOne(ext: DiscoveredExtension, delegate: HostDelegate): Promise<Loaded> {
        const { manifest, root } = ext;
        const spec: SpawnSpec = {
            command: manifest.run.command,
            args: manifest.run.args,
            env: resolvedEnv(manifest),
            cwd: root,
        };
        const handler = new HostInbound(manifest.name, delegate, () => this.epoch);
        const proc = ExtensionProcess.spawn(spec, handler);
        const init = await this.initialize(proc);
        const subscriptions = effectiveSubscriptions(manifest.capabilities.events, init.registrations?.subscriptions ?? []);
        return {
            name: manifest.name,
            process: proc,
            init,
            subscriptions,
            declaredEvents: manifest.capabilities.events,
            hookTimeoutMs: manifest.hookTimeoutMs,
        };
    }

    /** Send `initialize` and parse the registrations. Shared by load and reload. */
    private async initialize(proc: ExtensionProcess): Promise<InitializeResult> {
        const params: InitializeParams = {
            protocol_version: PROTOCOL_VERSION,
            host: this.host,
            workspace: this.workspace,
            mode: this.mode,
            ui_capabilities: this.uiCapabilities,
            flags: {},
        };
        const raw = (await proc.request(method.INITIALIZE, params, 10_000)) as InitializeResult;
        if (typeof raw?.protocol_version !== 'number' || typeof raw?.extension?.name !== 'string') {
            throw new Error('bad initialize result');
        }
        return raw;
    }

    /** Number of successfully loaded extensions. */
    get length(): number {
        return this.extensions.length;
    }

    isEmpty(): boolean {
        return this.extensions.length === 0;
    }

    /** Names of loaded extensions, in load order. */
    names(): string[] {
        return this.extensions.map((e) => e.name);
    }

    /**
     * A fresh dispatch context. Session-mutating actions need `command` tier. The
     * token embeds the current epoch so it is invalidated across reloads.
     */
    context(tier: Tier): Context {
        return { token: `epoch-${this.epoch}`, tier };
    }

    /** Bump the epoch, invalidating every previously minted context token. */
    bumpEpoch(): void {
        this.epoch++;
    }

    /** True if any loaded extension subscribed to `event`. */
    hasSubscriber(event: string): boolean {
        return this.extensions.some((e) => e.subscriptions.has(event));
    }

    /**
     * Fire-and-forget event fanout to every subscribed extension. Non-blocking: a
     * slow or dead extension never stalls the caller (bounded, lossy observe lane).
     */
    dispatchEvent(event: string, payload: unknown): void {
        if (this.extensions.length === 0) return;
        const ctx = this.context('event');
        for (const ext of this.extensions) {
            if (!ext.subscriptions.has(event)) continue;
            ext.process.sendEvent(event, ctx, payload);
        }
    }

    /**
     * Run a hook across every extension in load order, folding the chain. Each
     * extension sees the prior extension's patch. Fail-open/closed per hook type.
     */
    async runHook(hook: HookType, input: unknown): Promise<FoldedHook> {
        if (this.extensions.length === 0) return { kind: 'proceed', value: input };
        const ctx = this.context('command');
        let current = input;
        for (const ext of this.extensions) {
            const params = { hook, context: ctx, input: current };
            const timeout = ext.hookTimeoutMs ?? hookDefaultTimeoutMs(hook);
            let step: HookStep;
            try {
                const value = await ext.process.request(method.HOOK, params, timeout);
                try {
                    step = { kind: 'replied', outcome: parseHookOutcome(value) };
                } catch {
                    step = { kind: 'failed' };
                }
            } catch {
                step = { kind: 'failed' };
            }
            const folded = foldHookChain(hook, current, [step]);
            if (folded.kind === 'blocked') return folded;
            current = folded.value;
        }
        return { kind: 'proceed', value: current };
    }

    /** Convenience: run the `tool_call` hook (fail-closed) on a pending call. */
    runToolCallHook(tool: string, args: unknown): Promise<FoldedHook> {
        return this.runHook('tool_call', { tool, arguments: args });
    }

    /**
     * Run the `before_agent_start` hook on a system prompt, returning the
     * possibly-rewritten prompt. Fail-open: a blocked/failed hook leaves it unchanged.
     */
    async beforeAgentStart(systemPrompt: string): Promise<string> {
        if (this.extensions.length === 0) return systemPrompt;
        const folded = await this.runHook('before_agent_start', { system_prompt: systemPrompt });
        if (folded.kind === 'blocked') return systemPrompt;
        const v = (folded.value as { system_prompt?: unknown })?.system_prompt;
        return typeof v === 'string' ? v : systemPrompt;
    }

    /**
     * Tool proxies for every eager tool every extension registered. Names are dotted
     * `<ext>.<tool>`. Deferred tools are returned by {@link deferredTools}.
     */
    tools(): Tool[] {
        return this.collectTools(false);
    }

    /** Deferred tool proxies. */
    deferredTools(): Tool[] {
        return this.collectTools(true);
    }

    private collectTools(deferred: boolean): Tool[] {
        const ctx = this.context('command');
        const out: Tool[] = [];
        for (const ext of this.extensions) {
            for (const reg of ext.init.registrations?.tools ?? []) {
                if ((reg.deferred ?? false) !== deferred) continue;
                out.push(new ExtensionTool(ext.name, reg, ext.process, ctx));
            }
        }
        return out;
    }

    /**
     * Eager tool proxies for a single extension, minted at the CURRENT epoch. The
     * frontend calls this after a {@link reload} to re-register the reloaded
     * extension's tools (its old proxies carry a stale context).
     */
    toolsFor(extName: string): Tool[] {
        const ctx = this.context('command');
        const ext = this.extensions.find((e) => e.name === extName);
        if (!ext) return [];
        return (ext.init.registrations?.tools ?? []).filter((reg) => !(reg.deferred ?? false)).map((reg) => new ExtensionTool(ext.name, reg, ext.process, ctx));
    }

    /** Every registered slash-command across all extensions, paired with the owning extension name. */
    commands(): Array<[string, CommandRegistration]> {
        const out: Array<[string, CommandRegistration]> = [];
        for (const ext of this.extensions) {
            for (const cmd of ext.init.registrations?.commands ?? []) out.push([ext.name, cmd]);
        }
        return out;
    }

    /** Every keyboard shortcut across all extensions, paired with the owning extension name. */
    shortcuts(): Array<[string, ShortcutRegistration]> {
        const out: Array<[string, ShortcutRegistration]> = [];
        for (const ext of this.extensions) {
            for (const sc of ext.init.registrations?.shortcuts ?? []) out.push([ext.name, sc]);
        }
        return out;
    }

    private commandOwner(extName: string | undefined, command: string): ExtensionProcess | undefined {
        for (const ext of this.extensions) {
            if (extName !== undefined && extName !== ext.name) continue;
            if ((ext.init.registrations?.commands ?? []).some((c) => c.name === command)) return ext.process;
        }
        return undefined;
    }

    /**
     * Dispatch a registered slash-command to its owning extension with a COMMAND-tier
     * context. Pass `extName` to disambiguate a command registered by more than one
     * extension; `undefined` picks the first match in load order.
     */
    async runCommand(extName: string | undefined, command: string, args: unknown): Promise<CommandExecuteResult> {
        const proc = this.commandOwner(extName, command);
        if (!proc) throw new RpcError(codes.MethodNotFound, `no extension registered command \`${command}\``);
        const params = { command, context: this.context('command'), arguments: args };
        return (await proc.request(method.COMMAND_EXECUTE, params, 120_000)) as CommandExecuteResult;
    }

    /**
     * Ask the extension that owns `command` for argument completions given the
     * `partial` text typed so far. Returns an empty list on error (best-effort —
     * never fail the caller's keystroke).
     */
    async completeCommand(extName: string | undefined, command: string, partial: string): Promise<Completion[]> {
        const proc = this.commandOwner(extName, command);
        if (!proc) return [];
        const params = { command, context: this.context('command'), partial };
        try {
            const raw = (await proc.request(method.COMMAND_COMPLETE, params, 5_000)) as CommandCompleteResult;
            return raw?.completions ?? [];
        } catch {
            return [];
        }
    }

    /**
     * Hot-reload a single extension by name: notify it (`session_shutdown` reason
     * `reload`), bump the epoch so every context token it still holds is invalidated,
     * respawn its subprocess (the generation guard discards any late reply), re-run
     * `initialize`, then notify it (`session_start` reason `reload`). The caller
     * re-registers the extension's tools via {@link toolsFor}.
     */
    async reload(name: string): Promise<void> {
        const ext = this.extensions.find((e) => e.name === name);
        if (!ext) throw new Error(`extension \`${name}\` is not loaded`);
        ext.process.sendEvent('session_shutdown', this.context('event'), { reason: 'reload' });
        this.bumpEpoch();
        ext.process.respawn();
        const init = await this.initialize(ext.process);
        ext.subscriptions = effectiveSubscriptions(ext.declaredEvents, init.registrations?.subscriptions ?? []);
        ext.init = init;
        ext.process.sendEvent('session_start', this.context('event'), { reason: 'reload' });
    }

    /** Gracefully shut down every extension (5s grace each, then SIGKILL). */
    async shutdownAll(): Promise<void> {
        await Promise.all(this.extensions.map((e) => e.process.shutdown(5_000)));
    }
}
