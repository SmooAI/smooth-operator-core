/**
 * Extension manifests — `extension.toml` discovery, merge, and `${env:VAR}`
 * expansion. The TS sibling of the Rust host's `extension/manifest.rs`.
 *
 * - An extension lives in a directory holding an `extension.toml`.
 * - Global extensions: `~/.smooth/extensions/<name>/extension.toml`
 *   (or `$SMOOTH_HOME/extensions/...`).
 * - Project extensions: `<workspace>/.smooth/extensions/<name>/extension.toml`.
 * - On a name collision the **project entry wins** (the mcp.toml / plugin.toml
 *   merge rule).
 * - `[run] env` values support `${env:VAR}` expansion so secrets stay out of the
 *   manifest.
 * - A single malformed manifest is tolerated: it is collected as a failure and
 *   the rest still load.
 */

import { readdirSync, readFileSync, statSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { parse as parseToml } from 'smol-toml';

/**
 * Where a manifest was discovered. Project extensions only load in trusted
 * workspaces; the host uses this to apply that policy.
 */
export type Scope = 'global' | 'project';

/** How to launch the extension subprocess. */
export interface RunSpec {
    /** Executable to spawn (e.g. `node`, `python3`, an absolute path). */
    command: string;
    args: string[];
    /** Extra env vars; values may reference `${env:VAR}`. */
    env: Record<string, string>;
}

/**
 * Capability declarations. The `events` list doubles as the host's dispatch
 * filter — an extension only receives events it names here.
 */
export interface Capabilities {
    events: string[];
    tools: boolean;
    commands: boolean;
    ui: boolean;
    exec: boolean;
    kv: boolean;
    bus: boolean;
    session: boolean;
}

/** Resource directories the extension contributes (skills, prompts, themes). */
export interface Resources {
    skills?: string;
    prompts?: string;
    themes?: string;
}

/** A parsed `extension.toml`. */
export interface ExtensionManifest {
    name: string;
    version: string;
    /** Highest SEP protocol version the extension declares. Defaults to 1. */
    protocol: number;
    run: RunSpec;
    capabilities: Capabilities;
    resources: Resources;
    /** Per-extension hook timeout override, in milliseconds. */
    hookTimeoutMs?: number;
    /** Optional: skip this extension without deleting its manifest. */
    disabled: boolean;
}

/**
 * A discovered extension: its manifest plus the directory it was found in
 * (relative resources and `args` resolve against this root) and its scope.
 */
export interface DiscoveredExtension {
    manifest: ExtensionManifest;
    root: string;
    scope: Scope;
}

/** A `(source, error message)` pair for a manifest that failed to parse. */
export type ManifestFailure = [source: string, error: string];

function asString(v: unknown, fallback = ''): string {
    return typeof v === 'string' ? v : fallback;
}

function asStringArray(v: unknown): string[] {
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [];
}

function asBool(v: unknown): boolean {
    return v === true;
}

/**
 * Parse a manifest from TOML text. Throws when the TOML is malformed or the
 * required `name` / `version` / `[run] command` fields are missing — the same
 * required set the Rust serde struct enforces.
 */
export function parseManifest(tomlText: string): ExtensionManifest {
    const raw = parseToml(tomlText) as Record<string, unknown>;
    const name = asString(raw.name);
    const version = asString(raw.version);
    if (!name || !version) throw new Error('extension.toml: `name` and `version` are required');

    const runRaw = (raw.run ?? {}) as Record<string, unknown>;
    const command = asString(runRaw.command);
    if (!command) throw new Error('extension.toml: `[run] command` is required');
    const envRaw = (runRaw.env ?? {}) as Record<string, unknown>;
    const env: Record<string, string> = {};
    for (const [k, val] of Object.entries(envRaw)) env[k] = asString(val, String(val));

    const capRaw = (raw.capabilities ?? {}) as Record<string, unknown>;
    const resRaw = (raw.resources ?? {}) as Record<string, unknown>;

    const manifest: ExtensionManifest = {
        name,
        version,
        protocol: typeof raw.protocol === 'number' ? raw.protocol : 1,
        run: { command, args: asStringArray(runRaw.args), env },
        capabilities: {
            events: asStringArray(capRaw.events),
            tools: asBool(capRaw.tools),
            commands: asBool(capRaw.commands),
            ui: asBool(capRaw.ui),
            exec: asBool(capRaw.exec),
            kv: asBool(capRaw.kv),
            bus: asBool(capRaw.bus),
            session: asBool(capRaw.session),
        },
        resources: {
            skills: typeof resRaw.skills === 'string' ? resRaw.skills : undefined,
            prompts: typeof resRaw.prompts === 'string' ? resRaw.prompts : undefined,
            themes: typeof resRaw.themes === 'string' ? resRaw.themes : undefined,
        },
        disabled: asBool(raw.disabled),
    };
    if (typeof raw.hook_timeout_ms === 'number') manifest.hookTimeoutMs = raw.hook_timeout_ms;
    return manifest;
}

/** Load a manifest from `<dir>/extension.toml`. Throws if missing or malformed. */
export function loadManifestDir(dir: string): ExtensionManifest {
    const path = join(dir, 'extension.toml');
    return parseManifest(readFileSync(path, 'utf8'));
}

/**
 * Return the `[run] env` map with `${env:VAR}` references expanded against the
 * host's current environment. Unset variables expand to empty strings.
 */
export function resolvedEnv(manifest: ExtensionManifest): Record<string, string> {
    const out: Record<string, string> = {};
    for (const [k, v] of Object.entries(manifest.run.env)) out[k] = expandEnv(v);
    return out;
}

/**
 * Default global extensions directory: `$SMOOTH_HOME/extensions` if set, else
 * `~/.smooth/extensions`.
 */
export function defaultGlobalDir(): string | undefined {
    const home = process.env.SMOOTH_HOME;
    if (home) return join(home, 'extensions');
    const h = homedir();
    return h ? join(h, '.smooth', 'extensions') : undefined;
}

/** The project extensions directory for a workspace root. */
export function projectDir(workspaceRoot: string): string {
    return join(workspaceRoot, '.smooth', 'extensions');
}

/**
 * Discover every extension under `globalDir` and `projectDir`, merging by name
 * with **project winning**. Either directory may be `undefined` or missing
 * (treated as empty). Returns the chosen extensions plus a list of
 * `(source, error)` for manifests that failed to parse — a single bad manifest
 * never aborts discovery.
 */
export function discover(globalDir: string | undefined, projectDirPath: string | undefined): { extensions: DiscoveredExtension[]; failures: ManifestFailure[] } {
    const failures: ManifestFailure[] = [];
    const byName = new Map<string, DiscoveredExtension>();

    // Global first, then project, so project overwrites on name collision.
    for (const [dir, scope] of [
        [globalDir, 'global'],
        [projectDirPath, 'project'],
    ] as const) {
        if (!dir) continue;
        for (const found of scanDir(dir, scope, failures)) byName.set(found.manifest.name, found);
    }

    // Stable order so load-order-dependent hook chaining is deterministic.
    const extensions = [...byName.values()].sort((a, b) => a.manifest.name.localeCompare(b.manifest.name));
    return { extensions, failures };
}

/**
 * Scan a single extensions directory: each immediate subdirectory holding an
 * `extension.toml` is one extension.
 */
function scanDir(dir: string, scope: Scope, failures: ManifestFailure[]): DiscoveredExtension[] {
    const out: DiscoveredExtension[] = [];
    let entries: string[];
    try {
        entries = readdirSync(dir);
    } catch {
        // Missing dir is not an error — just no extensions from this scope.
        return out;
    }
    for (const entry of entries) {
        const root = join(dir, entry);
        try {
            if (!statSync(root).isDirectory()) continue;
            if (!statSync(join(root, 'extension.toml')).isFile()) continue;
        } catch {
            continue;
        }
        try {
            out.push({ manifest: loadManifestDir(root), root, scope });
        } catch (e) {
            failures.push([root, e instanceof Error ? e.message : String(e)]);
        }
    }
    return out;
}

/**
 * Expand `${env:VAR}` references using the host's current environment. Unset
 * variables expand to empty strings. Mirrors the Rust/MCP loader so the two
 * config surfaces behave identically.
 */
export function expandEnv(input: string): string {
    let out = '';
    let rest = input;
    for (;;) {
        const idx = rest.indexOf('${env:');
        if (idx === -1) break;
        out += rest.slice(0, idx);
        const after = rest.slice(idx + 6);
        const end = after.indexOf('}');
        if (end === -1) {
            // Unterminated reference — keep it verbatim and stop.
            out += rest.slice(idx);
            return out;
        }
        const varName = after.slice(0, end);
        out += process.env[varName] ?? '';
        rest = after.slice(end + 1);
    }
    return out + rest;
}
