/**
 * Native tool-call permission gate for the TypeScript engine.
 *
 * The TypeScript sibling of the Rust reference engine's `permission.rs`. A
 * {@link PermissionHook} runs the pure, deterministic {@link decide} classifier
 * on every tool call and blocks (throws) on a **Deny**. An **Ask** is routed to
 * a human approver (the existing {@link HumanGate} seam) when one is wired, and
 * **fails closed** (blocks) when it is not.
 *
 * The classification model is ported natively from smooth's `auto_mode` /
 * `smooth-narc::judge`. This is the security-critical core and is exhaustively
 * tested — including adversarial compound-command and credential-path inputs.
 *
 * A stored grant ({@link PermissionGrants}) auto-approves a matching `Ask`
 * without prompting, and answering "approve always" persists a new grant. A
 * grant can only upgrade an `Ask` — it can **never** waive a `Deny`
 * circuit-breaker. A consumer-supplied {@link DenyPolicy} is evaluated FIRST and
 * is a circuit-breaker too: no grant waives it and no mode downgrades it.
 */

import type { DenyPolicy } from './denyPolicy.js';
import type { HumanGate } from './humanGate.js';
import { isApproved } from './humanGate.js';
import { PermissionGrants } from './permissionGrants.js';
import type { GrantQuery } from './permissionGrants.js';

// ToolCall/ToolHook are the engine-wide hook seam, defined once in agent.ts
// (optional preCall/postCall). PermissionHook implements the preCall gate.
import type { ToolCall, ToolHook } from './agent.js';

export type { ToolCall, ToolHook };

/**
 * How aggressively the hook enforces. Mirrors the Rust engine's `AutoMode` (a
 * trimmed Claude Code `auto-mode` set). Selected via the `SMOOTH_AUTO_MODE` env
 * var through {@link autoModeFromEnv}.
 */
export enum AutoMode {
    /** Read-only allow, mutating ask, dangerous deny. Default. */
    Ask = 'ask',
    /** Like {@link AutoMode.Ask} but filesystem-edit (`Write`) tools auto-approve. Mirrors `acceptEdits`. */
    AcceptEdits = 'accept-edits',
    /** Like {@link AutoMode.Ask} but an unmatched verdict is a deny (fail-closed). Headless / CI posture (`dontAsk`). */
    DenyUnmatched = 'deny-unmatched',
    /** Allow everything **except** the hard circuit-breakers. Escape hatch (`bypassPermissions`, which keeps its breakers). */
    Bypass = 'bypass',
}

/** Parse a `SMOOTH_AUTO_MODE` value. Unknown / unset ⇒ {@link AutoMode.Ask}. */
export function autoModeFromValue(v: string | undefined): AutoMode {
    switch (
        v
            ?.trim()
            .toLowerCase()
            .replace(/[-_]/g, '')
    ) {
        case 'deny':
        case 'denyunmatched':
        case 'dontask':
        case 'headless':
            return AutoMode.DenyUnmatched;
        case 'bypass':
        case 'bypasspermissions':
        case 'yolo':
            return AutoMode.Bypass;
        case 'acceptedits':
        case 'acceptedit':
        case 'edits':
            return AutoMode.AcceptEdits;
        default:
            return AutoMode.Ask;
    }
}

/** Read the mode from the process `SMOOTH_AUTO_MODE` environment variable. */
export function autoModeFromEnv(): AutoMode {
    return autoModeFromValue(process.env.SMOOTH_AUTO_MODE);
}

/** The pure verdict returned by {@link decide}. A discriminated union on `kind`. */
export type Verdict = { kind: 'allow' } | { kind: 'deny'; reason: string } | { kind: 'ask'; reason: string };

const ALLOW: Verdict = { kind: 'allow' };
const deny = (reason: string): Verdict => ({ kind: 'deny', reason });
const ask = (reason: string): Verdict => ({ kind: 'ask', reason });

// ---------------------------------------------------------------------------
// Circuit-breaker data (ported from smooth-narc::judge + auto_mode)
// ---------------------------------------------------------------------------

/** Domains we never auto-approve — suffix match, case-insensitive. */
const DANGEROUS_DOMAIN_SUFFIXES: readonly string[] = [
    '.ngrok.io',
    '.ngrok-free.app',
    'etherscan.io',
    'blockchain.info',
    'binance.com',
    'pastebin.com',
    'termbin.com',
    'transfer.sh',
];

/** Shell substrings that must never run — checked case-insensitively against each subcommand. */
const DANGEROUS_CLI_SUBSTRINGS: readonly string[] = [
    'rm -rf /',
    'rm -rf ~',
    ':(){ :|:& };:',
    'mkfs',
    'dd if=/dev/zero of=/dev/',
    '> /dev/sda',
    'chmod -r 777 /',
    '| sudo sh',
    'systemctl mask',
];

/** Substrings meaning "this command touches a credential / sensitive path". A match is an immediate deny. */
const SENSITIVE_PATH_SUBSTRINGS: readonly string[] = [
    '.ssh/',
    '.aws/credentials',
    '.aws/config',
    '.config/gh/',
    '.config/gcloud',
    '.gnupg',
    '.kube/config',
    '.docker/config.json',
    '.npmrc',
    '.pypirc',
    '.netrc',
    '/etc/shadow',
    'id_rsa',
    'id_ed25519',
    '.smooth/providers.json',
    '.smooth/auth/',
];

/** Read-only command binaries that are always safe. */
const SAFE_BASH_BINS: readonly string[] = [
    'ls',
    'cat',
    'head',
    'tail',
    'wc',
    'grep',
    'rg',
    'fd',
    'find',
    'echo',
    'pwd',
    'which',
    'whoami',
    'date',
    'true',
    'test',
    'dirname',
    'basename',
    'realpath',
    'stat',
    'file',
    'cksum',
    'sha256sum',
    'md5sum',
];

/** `git` subcommands that only read. */
const SAFE_GIT_SUBCOMMANDS: readonly string[] = ['status', 'log', 'diff', 'show', 'branch', 'remote', 'rev-parse', 'describe', 'blame', 'ls-files'];

/** Flags under which `git branch` / `git remote` stay read-only. */
const GIT_LIST_ONLY_FLAGS: readonly string[] = ['-a', '-r', '-v', '-vv', '--all', '--list', '--verbose', '--show-current', '--merged', '--no-merged'];

/** Binaries that make outbound network requests. */
const NET_BASH_BINS: readonly string[] = ['curl', 'wget', 'http', 'https', 'nc', 'ncat', 'telnet'];

/** Shell interpreters that execute piped stdin — the sink half of a `curl … | sh`. */
const SHELL_INTERPRETERS: readonly string[] = ['sh', 'bash', 'zsh', 'dash', 'ksh'];

/** Env-var name fragments whose `$NAME` expansion is treated as secret exfiltration. Substring, case-insensitive. */
const SENSITIVE_VAR_FRAGMENTS: readonly string[] = [
    'secret',
    'token',
    'password',
    'passwd',
    'api_key',
    'apikey',
    'access_key',
    'credential',
    'private_key',
    'aws_',
    'ssh_',
    'session',
];

/** Transparent command wrappers that don't change what runs. */
const WRAPPERS: readonly string[] = ['timeout', 'nice', 'nohup', 'stdbuf', 'env'];

/** Split whitespace, dropping empty tokens (the `split_whitespace` equivalent). */
function tokenize(s: string): string[] {
    return s.split(/\s+/).filter((t) => t.length > 0);
}

/** Pull a string argument by any of the given keys, or `''` when absent. */
function strArg(args: Record<string, unknown>, keys: readonly string[]): string {
    for (const k of keys) {
        const v = args[k];
        if (typeof v === 'string') return v;
    }
    return '';
}

/** Match a domain against a suffix list (exact or subdomain), case-insensitive. */
export function domainMatchesSuffixList(domain: string, suffixes: readonly string[]): boolean {
    const d = domain.toLowerCase();
    return suffixes.some((suffix) => {
        const s = suffix.toLowerCase();
        return d === s || d.endsWith(`.${s}`) || (s.startsWith('.') && d.endsWith(s));
    });
}

/**
 * Split a shell command line into subcommands on the operators that sequence
 * independent commands: `&&`, `||`, `;`, `|`, `&`, and newlines. Command /
 * process substitution (`$(…)`, `` `…` ``, `<(…)`) is surfaced as its own
 * segment so it can't ride in on a safe outer command.
 *
 * ponytail: substring split, not a real shell lexer — upgrade only if quoting
 * edge-cases (`echo "a && b"`) start mattering for policy.
 */
export function splitCompound(command: string): string[] {
    let normalized = command.replace(/&&/g, '').replace(/\|\|/g, '');
    if (normalized.includes('$(') || normalized.includes('<(') || normalized.includes('`')) {
        normalized = normalized
            .replace(/\$\(/g, '')
            .replace(/<\(/g, '')
            .replace(/[`)]/g, '');
    }
    return normalized
        .split(/[;|&\n]/)
        .map((s) => s.trim().replace(/^["']+|["']+$/g, '').trim())
        .filter((s) => s.length > 0);
}

/** Strip leading command wrappers; returns the index of the real command token. */
function stripWrappers(tokens: string[]): number {
    let i = 0;
    while (i < tokens.length && WRAPPERS.includes(tokens[i])) {
        i += 1;
        while (i < tokens.length && (tokens[i].startsWith('-') || /^[0-9]/.test(tokens[i]))) i += 1;
    }
    return i;
}

/** First meaningful token of a subcommand (after stripping wrappers). */
function commandBin(subcommand: string): string | undefined {
    const tokens = tokenize(subcommand);
    const start = stripWrappers(tokens);
    return tokens[start];
}

/** Pull a bare hostname out of a URL-ish or `host:port` token. */
export function hostFromToken(tok: string): string | undefined {
    const schemeIdx = tok.indexOf('://');
    const afterScheme = schemeIdx >= 0 ? tok.slice(schemeIdx + 3) : tok;
    const at = afterScheme.lastIndexOf('@');
    const afterUserinfo = at >= 0 ? afterScheme.slice(at + 1) : afterScheme;
    const host = (afterUserinfo.split(/[/:?#]/)[0] ?? '').trim();
    if (host.length === 0) return undefined;
    if (host === 'localhost' || (host.includes('.') && !host.startsWith('.') && !host.endsWith('.'))) {
        return host.toLowerCase();
    }
    return undefined;
}

/** Extract candidate hostnames from a single (already split) net-tool subcommand. */
export function extractHosts(subcommand: string): string[] {
    const tokens = tokenize(subcommand);
    const start = stripWrappers(tokens);
    const bin = tokens[start];
    if (bin === undefined || !NET_BASH_BINS.includes(bin)) return [];
    return tokens
        .slice(start + 1)
        .filter((t) => !t.startsWith('-'))
        .map(hostFromToken)
        .filter((h): h is string => h !== undefined);
}

/** The effective binary of a pipe segment, skipping a leading `sudo` and the usual wrappers. */
function sinkBin(segment: string): string | undefined {
    const tokens = tokenize(segment);
    let i = stripWrappers(tokens);
    while (i < tokens.length && tokens[i] === 'sudo') {
        i += 1;
        while (i < tokens.length && tokens[i].startsWith('-')) i += 1;
    }
    return tokens[i];
}

/**
 * Does this whole command line pipe a network fetch into a shell interpreter
 * (`curl … | sh`)? A hard circuit-breaker regardless of the specific host.
 */
function isPipeToShell(command: string): boolean {
    if (!command.includes('|')) return false;
    let sawFetch = false;
    for (const seg of command.split('|')) {
        const bin = sinkBin(seg.trim());
        if (bin === undefined) continue;
        if (sawFetch && SHELL_INTERPRETERS.includes(bin)) return true;
        if (NET_BASH_BINS.includes(bin)) sawFetch = true;
    }
    return false;
}

/**
 * Strip leading transparent wrappers and any leading `sudo` from a single
 * subcommand, returning the remaining command text. Used by the deny policy so a
 * rule anchored on the real binary (`aws …`) still matches `sudo aws …` /
 * `timeout 5 aws …`.
 */
export function stripWrappersAndSudo(subcommand: string): string {
    const tokens = tokenize(subcommand);
    let i = stripWrappers(tokens);
    while (i < tokens.length && tokens[i] === 'sudo') {
        i += 1;
        while (i < tokens.length && tokens[i].startsWith('-')) i += 1;
    }
    return tokens.slice(i).join(' ');
}

/** Does the command reference a sensitive credential path? */
function referencesSensitivePath(command: string): boolean {
    const lower = command.toLowerCase();
    if (SENSITIVE_PATH_SUBSTRINGS.some((p) => lower.includes(p.toLowerCase()))) return true;
    // `.env` / `.envrc` / `.env.local` dotenv files are secret stores too.
    // Token-scoped so `rg "process.env" src/` isn't flagged.
    return lower.split(/\s+/).some((t) => {
        const tt = t.replace(/^[";'()]+|[";'()]+$/g, '');
        return tt.startsWith('.env') || tt.includes('/.env');
    });
}

/** True if the text contains a `$NAME` / `${NAME}` expansion whose name matches a sensitive fragment. */
function containsSensitiveVarExpansion(text: string): boolean {
    const lower = text.toLowerCase();
    let idx = 0;
    for (;;) {
        const rel = lower.indexOf('$', idx);
        if (rel < 0) break;
        let j = rel + 1;
        if (lower[j] === '{') j += 1;
        const nameStart = j;
        while (j < lower.length && /[a-z0-9_]/.test(lower[j])) j += 1;
        const name = lower.slice(nameStart, j);
        if (name.length > 0 && SENSITIVE_VAR_FRAGMENTS.some((f) => name.includes(f))) return true;
        idx = rel + 1;
    }
    return false;
}

/**
 * Does this single (already split) subcommand reveal the process environment?
 * Matches on intent, not a single binary name. Does NOT match the legitimate
 * setter forms (`env FOO=bar cmd`, `export FOO=bar`, `set -euo pipefail`).
 */
function dumpsEnvironment(subcommand: string): boolean {
    const toks = tokenize(subcommand);
    if (toks.length === 0) return false;
    const lower = subcommand.toLowerCase();
    if (lower.includes('proc/') && lower.includes('/environ')) return true;
    // Skip transparent wrappers (but NOT `env`, the subject here).
    let i = 0;
    while (i < toks.length && (toks[i] === 'timeout' || toks[i] === 'nice' || toks[i] === 'nohup' || toks[i] === 'stdbuf')) {
        i += 1;
        while (i < toks.length && (toks[i].startsWith('-') || /^[0-9]/.test(toks[i]))) i += 1;
    }
    const bin = toks[i];
    if (bin === undefined) return false;
    const rest = toks.slice(i + 1);
    switch (bin) {
        case 'printenv':
            return true;
        case 'env': {
            let k = 0;
            while (k < rest.length) {
                const t = rest[k];
                if (t === '-u' || t === '-S') {
                    k += 2;
                } else if (t.startsWith('-') || t.includes('=') || t === '-') {
                    k += 1;
                } else {
                    return false; // a bare command token → setter form
                }
            }
            return true;
        }
        case 'export':
        case 'declare':
        case 'typeset':
            return !rest.some((t) => t.includes('=')) && rest.every((t) => t.startsWith('-'));
        case 'set':
            return rest.length === 0;
        case 'echo':
        case 'printf':
            return containsSensitiveVarExpansion(subcommand);
        default:
            return false;
    }
}

/** Is this single subcommand a compiled-in safe read-only command? */
function isSafeReadonlyBash(subcommand: string): boolean {
    const bin = commandBin(subcommand);
    if (bin === undefined) return false;
    if (bin === 'find') {
        const FIND_ACTION_FLAGS = ['-exec', '-execdir', '-ok', '-okdir', '-delete', '-fprint', '-fprintf', '-fls'];
        return !tokenize(subcommand).some((t) => FIND_ACTION_FLAGS.includes(t));
    }
    if (SAFE_BASH_BINS.includes(bin)) return true;
    if (bin === 'git') {
        const tokens = tokenize(subcommand);
        const start = stripWrappers(tokens);
        let j = start + 1;
        while (j < tokens.length && tokens[j].startsWith('-')) j += 2; // `-c key=val` / `-C dir`
        const sub = tokens[j];
        if (sub === undefined) return false;
        if (!SAFE_GIT_SUBCOMMANDS.includes(sub)) return false;
        if (sub === 'branch' || sub === 'remote') {
            return tokens.slice(j + 1).every((t) => GIT_LIST_ONLY_FLAGS.includes(t));
        }
        return true;
    }
    return false;
}

/** Evaluate a single bash subcommand against the layered policy. */
function decideBashSubcommand(subcommand: string): Verdict {
    if (referencesSensitivePath(subcommand)) {
        return deny(`command references a sensitive credential path: ${subcommand}`);
    }
    if (dumpsEnvironment(subcommand)) {
        return deny(`command reveals the process environment (secret exfiltration risk): ${subcommand}`);
    }
    const lower = subcommand.toLowerCase();
    const needle = DANGEROUS_CLI_SUBSTRINGS.find((n) => lower.includes(n.toLowerCase()));
    if (needle !== undefined) return deny(`command matches dangerous-cli pattern: ${needle}`);
    const hosts = extractHosts(subcommand);
    for (const host of hosts) {
        if (domainMatchesSuffixList(host, DANGEROUS_DOMAIN_SUFFIXES)) {
            return deny(`${host} is on the dangerous-domain deny list`);
        }
    }
    if (hosts.length > 0) {
        return ask(`outbound request to ${hosts[0]} needs approval`);
    }
    if (isSafeReadonlyBash(subcommand)) return ALLOW;
    const bin = commandBin(subcommand) ?? '';
    return ask(`\`${bin}\` is not a known-safe command`);
}

/** Evaluate a whole (possibly compound) bash command line. The strictest verdict wins (deny > ask > allow). */
function decideBash(command: string): Verdict {
    // Whole-line dangerous-substring scan FIRST — some breakers (the fork bomb,
    // `| sudo sh`) contain the very operators splitCompound divides on.
    const lowerLine = command.toLowerCase();
    const needle = DANGEROUS_CLI_SUBSTRINGS.find((n) => lowerLine.includes(n.toLowerCase()));
    if (needle !== undefined) return deny(`command matches dangerous-cli pattern: ${needle}`);
    if (isPipeToShell(command)) return deny(`pipe-to-shell execution is blocked: ${command}`);
    const subs = splitCompound(command);
    if (subs.length === 0) return deny('empty command');
    let pendingAsk: string | undefined;
    for (const sub of subs) {
        const v = decideBashSubcommand(sub);
        if (v.kind === 'deny') return v;
        if (v.kind === 'ask' && pendingAsk === undefined) pendingAsk = v.reason;
    }
    return pendingAsk === undefined ? ALLOW : ask(pendingAsk);
}

/** Category a tool falls into, derived from its name. Drives the default posture for non-bash tools. */
export type Category = 'bash' | 'network' | 'write' | 'safe' | 'unknown';

export function toolCategory(name: string): Category {
    // Extension tools are dotted `<ext>.<tool>`; classify on the bare tool name.
    const bare = name.includes('.') ? name.slice(name.lastIndexOf('.') + 1) : name;
    const n = bare.toLowerCase();
    if (n === 'bash' || n === 'shell' || n === 'shell_exec' || n === 'run_command') return 'bash';
    if (n.includes('write') || n.includes('edit') || n.includes('delete') || n.includes('remove') || n === 'apply_patch' || n === 'create_file') {
        return 'write';
    }
    if (n.includes('fetch') || n.includes('download') || n.startsWith('http')) return 'network';
    if (n.startsWith('read') || n.startsWith('list') || n.startsWith('get') || n.includes('search') || n === 'grep' || n === 'glob') return 'safe';
    return 'unknown';
}

function decideInner(toolName: string, args: Record<string, unknown>): Verdict {
    switch (toolCategory(toolName)) {
        case 'bash': {
            const cmd = strArg(args, ['cmd', 'command']).trim();
            if (cmd.length === 0) return deny('bash call with no command');
            return decideBash(cmd);
        }
        case 'safe': {
            // Read-only is not exfil-proof: the read path IS the exfil path.
            for (const key of ['path', 'file', 'dir', 'directory']) {
                const v = args[key];
                if (typeof v === 'string' && referencesSensitivePath(v)) {
                    return deny(`${toolName} targets a sensitive credential path: ${v}`);
                }
            }
            return ALLOW;
        }
        case 'network': {
            const url = strArg(args, ['url', 'host']);
            const host = hostFromToken(url) ?? url;
            if (host.length === 0) return deny(`${toolName} call with no url/host`);
            if (domainMatchesSuffixList(host, DANGEROUS_DOMAIN_SUFFIXES)) return deny(`${host} is on the dangerous-domain deny list`);
            return ask(`outbound request to ${host} needs approval`);
        }
        case 'write': {
            const path = strArg(args, ['path', 'file']);
            if (referencesSensitivePath(path)) return deny(`write to a sensitive credential path: ${path}`);
            return ask(`\`${toolName}\` mutates the filesystem`);
        }
        case 'unknown':
            return ask(`\`${toolName}\` is not a recognised safe tool`);
    }
}

/**
 * The pure, deterministic permission decision. No async, no I/O — the
 * security-critical core, tested exhaustively.
 *
 * `args` is the raw tool-call argument object; the relevant field is pulled per
 * category (`cmd` for bash, `path` for writes, `url`/`host` for network).
 */
export function decide(mode: AutoMode, toolName: string, args: Record<string, unknown>): Verdict {
    const raw = decideInner(toolName, args);
    // Deny always survives, every mode.
    if (raw.kind === 'deny') return raw;
    // Bypass downgrades any surviving Ask to Allow (breakers already denied above).
    if (mode === AutoMode.Bypass) return ALLOW;
    if (mode === AutoMode.AcceptEdits && raw.kind === 'ask' && toolCategory(toolName) === 'write') return ALLOW;
    if (mode === AutoMode.DenyUnmatched && raw.kind === 'ask') return deny(`headless (no interactive approver): ${raw.reason}`);
    return raw;
}

// ---------------------------------------------------------------------------
// Grant derivation — map an `Ask` to a persistable grant and check whether a
// stored grant already covers it. Never derives from a `Deny`.
// ---------------------------------------------------------------------------

/** The grant a single asking bash subcommand maps to. */
function bashSegmentGrant(sub: string): GrantQuery {
    const host = extractHosts(sub)[0];
    if (host !== undefined) return { kind: 'network', host };
    return { kind: 'bash', prefix: `${commandBin(sub) ?? ''} ` };
}

/**
 * The grant that "approve always" would persist for this tool call, or
 * `undefined` when the call is not an `Ask`.
 */
function grantQuery(toolName: string, args: Record<string, unknown>): GrantQuery | undefined {
    switch (toolCategory(toolName)) {
        case 'bash': {
            const cmd = strArg(args, ['cmd', 'command']).trim();
            for (const sub of splitCompound(cmd)) {
                const v = decideBashSubcommand(sub);
                if (v.kind === 'ask') return bashSegmentGrant(sub);
                if (v.kind === 'deny') return undefined; // a deny sinks the line
            }
            return undefined;
        }
        case 'network': {
            const url = strArg(args, ['url', 'host']);
            const host = hostFromToken(url) ?? url;
            return host.length > 0 ? { kind: 'network', host } : undefined;
        }
        case 'write':
        case 'unknown':
            return { kind: 'tool', tool: toolName };
        case 'safe':
            return undefined;
    }
}

/** Is a single asking bash subcommand covered by a stored grant? */
function bashSegmentGranted(sub: string, grants: PermissionGrants): boolean {
    const host = extractHosts(sub)[0];
    if (host !== undefined) return grants.matchesHost(host);
    return grants.matchesBash(sub);
}

/**
 * Is this whole tool call already covered by stored grants? For compound bash,
 * **every** asking segment must be granted (a granted first segment must not
 * silently waive an ungranted second one).
 */
function coveredByGrants(grants: PermissionGrants, toolName: string, args: Record<string, unknown>): boolean {
    switch (toolCategory(toolName)) {
        case 'bash': {
            const cmd = strArg(args, ['cmd', 'command']).trim();
            const subs = splitCompound(cmd);
            if (subs.length === 0) return false;
            return subs.every((sub) => {
                const v = decideBashSubcommand(sub);
                if (v.kind === 'allow') return true;
                if (v.kind === 'deny') return false; // never auto-allow a deny
                return bashSegmentGranted(sub, grants);
            });
        }
        case 'network': {
            const url = strArg(args, ['url', 'host']);
            const host = hostFromToken(url) ?? url;
            return host.length > 0 && grants.matchesHost(host);
        }
        case 'write':
        case 'unknown':
            return grants.matchesTool(toolName);
        case 'safe':
            return false;
    }
}

// ---------------------------------------------------------------------------
// The hook
// ---------------------------------------------------------------------------

/**
 * A {@link ToolHook} that enforces {@link decide} on every tool call.
 *
 * **`Ask` routing**: with an approver wired via {@link PermissionHook.withApprover}
 * (the existing {@link HumanGate} seam), an `Ask` verdict prompts a human and
 * blocks until they approve; with no approver it **fails closed** (throws).
 * {@link AutoMode.Bypass} / {@link AutoMode.AcceptEdits} downgrade eligible asks
 * inside {@link decide} before they reach the approver. A `Deny` always blocks
 * and is never routed to the human — circuit-breakers are not waivable. A
 * consumer {@link DenyPolicy}, when attached, is evaluated FIRST and is a
 * circuit-breaker of the same tier.
 */
export class PermissionHook implements ToolHook {
    private approver?: HumanGate;
    private grants?: PermissionGrants;
    private denyPolicyRef?: DenyPolicy;

    constructor(private readonly autoMode: AutoMode = AutoMode.Ask) {}

    /** Build a hook reading the mode from `SMOOTH_AUTO_MODE` (default `Ask`). */
    static fromEnv(): PermissionHook {
        return new PermissionHook(autoModeFromEnv());
    }

    /**
     * Wire an interactive approver. When set, an `Ask` verdict consults the
     * {@link HumanGate} and blocks on its response — approve lets the call run,
     * anything else blocks it. Answering with `remember: true`
     * ({@link approveAlways}) persists a grant when {@link withGrants} is set.
     */
    withApprover(gate: HumanGate): this {
        this.approver = gate;
        return this;
    }

    /**
     * Wire the in-memory allow-list. A matching grant auto-approves an `Ask`
     * *before* prompting; an `approve always` answer adds a fresh grant so the
     * next identical `Ask` is silent. The consumer owns persisting `grants`.
     */
    withGrants(grants: PermissionGrants): this {
        this.grants = grants;
        return this;
    }

    /**
     * Attach a consumer {@link DenyPolicy}. Purely additive: with none attached
     * enforcement is identical to before. When set it is evaluated **first** — a
     * match is a hard deny (circuit-breaker tier) no grant can waive and no
     * {@link AutoMode} can downgrade.
     */
    withDenyPolicy(policy: DenyPolicy): this {
        this.denyPolicyRef = policy;
        return this;
    }

    /** The mode this hook enforces. */
    get mode(): AutoMode {
        return this.autoMode;
    }

    async preCall(call: ToolCall): Promise<void> {
        // Deny policy runs FIRST — a consumer deny is a circuit-breaker that wins
        // over grants, ask, allow, and every mode (Bypass included). Never routed
        // to a human, never grantable.
        if (this.denyPolicyRef) {
            const reason = this.denyPolicyRef.evaluate(call);
            if (reason !== undefined) throw new Error(`permission denied: ${reason}`);
        }
        const verdict = decide(this.autoMode, call.name, call.arguments);
        if (verdict.kind === 'allow') return;
        // Deny is a circuit-breaker — never routed to a human, never grantable.
        if (verdict.kind === 'deny') throw new Error(`permission denied: ${verdict.reason}`);
        // Ask: consult the persisted allow-list FIRST — a stored grant auto-approves silently.
        if (this.grants && coveredByGrants(this.grants, call.name, call.arguments)) return;
        if (this.approver === undefined) {
            throw new Error(`permission requires approval (fail-closed, no approver): ${verdict.reason}`);
        }
        const response = await this.approver({
            toolName: call.name,
            arguments: call.arguments,
            prompt: `Permission: ${verdict.reason}. Allow \`${call.name}\`?`,
        });
        if (!isApproved(response)) {
            throw new Error(`user denied: ${response.reason ?? 'no reason given'}`);
        }
        if (response.remember === true) this.persistGrant(call);
    }

    /** Add an approve-always grant to the in-memory allow-list. */
    private persistGrant(call: ToolCall): void {
        if (this.grants === undefined) return;
        const query = grantQuery(call.name, call.arguments);
        if (query !== undefined) this.grants.add(query);
    }
}
