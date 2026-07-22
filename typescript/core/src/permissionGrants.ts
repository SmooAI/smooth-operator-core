/**
 * Persistent permission grants — the allow-list (`wonk-allow.toml`).
 *
 * The TypeScript sibling of the Rust engine's `permission_grants.rs`. The
 * {@link PermissionHook} gate closes on an `Ask` verdict by prompting a human.
 * Without persistence that prompt is *approve-once*: the same command re-asks on
 * every run. This module ports smooth's `wonk-allow.toml` allow-list so a
 * human's "approve always" answer is remembered — a stored grant that matches a
 * later `Ask` auto-approves it **without prompting**.
 *
 * The on-disk schema is compatible with the Rust engine's `permission_grants`
 * (same TOML section names) so the files interoperate:
 *
 * ```toml
 * schema_version = 1
 *
 * [network]
 * allow_hosts = ["api.openai.com", "*.openai.com"]
 *
 * [tools]
 * allow = ["web_search", "vendor.file_write"]
 *
 * [bash]
 * allow_patterns = ["cargo ", "pnpm "]
 * ```
 *
 * - `network.allow_hosts` — exact host or `*.suffix` glob (case-insensitive).
 * - `tools.allow` — exact tool name (writes / unknown tools grant by name).
 * - `bash.allow_patterns` — a command *prefix*; the trailing space in `"cargo "`
 *   is significant (stops it matching `cargonaut`).
 *
 * There is no deny section: a stored grant can only upgrade an `Ask`, **never**
 * waive a `Deny` circuit-breaker (see {@link PermissionHook}).
 *
 * ponytail: in-memory + TOML round-trip only — no filesystem I/O helpers
 * (atomic writes / layered load / home-dir resolution). This crate is consumed
 * as a library; the CLI/daemon owns persistence and passes the parsed grants in.
 * Add fs helpers here only if a library consumer actually needs them.
 */

import { parse as parseToml, stringify as stringifyToml } from 'smol-toml';

/**
 * The kind of resource a grant covers — one of the three grantable `Ask` shapes.
 * (`Deny` circuit-breakers are never grantable.) A discriminated union.
 */
export type GrantQuery =
    | { kind: 'network'; host: string }
    | { kind: 'tool'; tool: string }
    | { kind: 'bash'; prefix: string };

/** The shape of a parsed `wonk-allow.toml` (snake_case for Rust interop). */
interface GrantsToml {
    schema_version?: number;
    network?: { allow_hosts?: string[] };
    tools?: { allow?: string[] };
    bash?: { allow_patterns?: string[] };
}

/**
 * In-memory allow-list. Case-insensitive matching for hosts and bash prefixes;
 * exact match for tool names.
 */
export class PermissionGrants {
    /** Always 1. Reserved for forward-compatible migrations. */
    schemaVersion = 1;
    readonly allowHosts = new Set<string>();
    readonly allowTools = new Set<string>();
    readonly allowBashPatterns = new Set<string>();

    /** True if `host` is covered by the network allow-list. */
    matchesHost(host: string): boolean {
        const lower = host.toLowerCase();
        for (const pat of this.allowHosts) {
            if (hostMatchesGlob(lower, pat)) return true;
        }
        return false;
    }

    /** True if `toolName` is in the tools allow-list (exact match). */
    matchesTool(toolName: string): boolean {
        return this.allowTools.has(toolName);
    }

    /** True if `command` starts with any bash allow prefix (case-insensitive). */
    matchesBash(command: string): boolean {
        const lower = command.toLowerCase();
        for (const p of this.allowBashPatterns) {
            if (lower.startsWith(p.toLowerCase())) return true;
        }
        return false;
    }

    /** True if `query`'s exact entry is already stored. */
    contains(query: GrantQuery): boolean {
        switch (query.kind) {
            case 'network':
                return this.matchesHost(query.host);
            case 'tool':
                return this.matchesTool(query.tool);
            case 'bash':
                return this.matchesBash(query.prefix);
        }
    }

    /** Add a grant. Idempotent. */
    add(query: GrantQuery): void {
        switch (query.kind) {
            case 'network':
                this.allowHosts.add(query.host);
                break;
            case 'tool':
                this.allowTools.add(query.tool);
                break;
            case 'bash':
                this.allowBashPatterns.add(query.prefix);
                break;
        }
    }

    /** Union `other` into `this`. */
    mergeWith(other: PermissionGrants): void {
        this.schemaVersion = Math.max(this.schemaVersion, other.schemaVersion);
        for (const h of other.allowHosts) this.allowHosts.add(h);
        for (const t of other.allowTools) this.allowTools.add(t);
        for (const p of other.allowBashPatterns) this.allowBashPatterns.add(p);
    }

    /** Parse from a TOML string. Missing sections default to empty. */
    static parse(tomlText: string): PermissionGrants {
        const raw = parseToml(tomlText) as GrantsToml;
        const g = new PermissionGrants();
        g.schemaVersion = typeof raw.schema_version === 'number' && raw.schema_version > 0 ? raw.schema_version : 1;
        for (const h of raw.network?.allow_hosts ?? []) g.allowHosts.add(h);
        for (const t of raw.tools?.allow ?? []) g.allowTools.add(t);
        for (const p of raw.bash?.allow_patterns ?? []) g.allowBashPatterns.add(p);
        return g;
    }

    /** Serialize to TOML. Empty sections are omitted. Entries are sorted for a stable round-trip. */
    toTomlString(): string {
        const out: GrantsToml = { schema_version: this.schemaVersion };
        if (this.allowHosts.size > 0) out.network = { allow_hosts: [...this.allowHosts].sort() };
        if (this.allowTools.size > 0) out.tools = { allow: [...this.allowTools].sort() };
        if (this.allowBashPatterns.size > 0) out.bash = { allow_patterns: [...this.allowBashPatterns].sort() };
        return stringifyToml(out as Record<string, unknown>);
    }
}

/**
 * Glob match for a single host pattern (case-insensitive):
 * - exact host: `api.example.com` matches only that.
 * - `*.example.com` / `.example.com`: any subdomain **and** the bare apex.
 * - a bare suffix (`example.com`) matches only itself (no substring match, so
 *   `evil-example.com` never slips past `example.com`).
 */
export function hostMatchesGlob(host: string, pattern: string): boolean {
    const h = host.toLowerCase();
    const p = pattern.toLowerCase();
    if (h === p) return true;
    if (p.startsWith('*.')) {
        const suffix = p.slice(2);
        return h.endsWith(`.${suffix}`) || h === suffix;
    }
    if (p.startsWith('.')) {
        const suffix = p.slice(1);
        return h.endsWith(`.${suffix}`) || h === suffix;
    }
    return false;
}
