/**
 * Consumer-supplied **deny policy** — the deny-side counterpart to
 * {@link PermissionGrants}. The TypeScript sibling of the Rust engine's
 * `deny_policy.rs`.
 *
 * The engine ships hardcoded circuit-breakers (`rm -rf /`, `curl | sh`,
 * credential paths, dangerous domains — see {@link decide}) and an allow-only
 * grant store that can *upgrade* an `Ask`. Neither can express a consumer's own
 * "never do this" rules: "never touch the prod AWS profile", "the DB writer
 * endpoint is off-limits", "no writes under `/prod`". This module adds that tier.
 *
 * It is **purely additive**: a {@link PermissionHook} with no deny policy behaves
 * exactly as before. When a policy *is* attached it is evaluated **first**, and a
 * match is a hard deny of the same tier as the built-in circuit-breakers — no
 * stored grant waives it, and {@link AutoMode.Bypass} / {@link AutoMode.AcceptEdits}
 * cannot downgrade it.
 *
 * # Two tiers
 *
 * 1. **Declarative** ({@link DenyRules}) — TOML, four sections, each a deny list:
 *
 *    ```toml
 *    schema_version = 1
 *    [tools]
 *    deny = ["vendor.dangerous_tool", "*.delete_prod"]
 *    [bash]
 *    deny_patterns = ["aws * --profile prod", "kubectl * --context prod"]
 *    [network]
 *    deny_hosts = ["*.prod.internal", "prod-*.rds.amazonaws.com"]
 *    [paths]
 *    deny = ["/prod/**", "/app/secrets/**"]
 *    ```
 *
 * 2. **Predicate** ({@link DenyPredicate}) — a consumer callback for semantic
 *    checks the engine cannot parse from strings ("is this the prod AWS account?",
 *    "writer vs replica endpoint?"). `Some(reason)` → deny.
 *
 * Declarative rules run first, then predicates; the first match wins.
 */

import { parse as parseToml, stringify as stringifyToml } from 'smol-toml';
import { domainMatchesSuffixList, extractHosts, hostFromToken, splitCompound, stripWrappersAndSudo, toolCategory } from './permission.js';
import type { ToolCall } from './permission.js';

/** The shape of a parsed deny-rules TOML (snake_case for Rust interop). */
interface DenyRulesToml {
    schema_version?: number;
    tools?: { deny?: string[] };
    bash?: { deny_patterns?: string[] };
    network?: { deny_hosts?: string[] };
    paths?: { deny?: string[] };
}

/** The declarative half of a {@link DenyPolicy}: four deny lists parsed from TOML. */
export class DenyRules {
    schemaVersion = 1;
    readonly tools = new Set<string>();
    readonly bashPatterns = new Set<string>();
    readonly networkHosts = new Set<string>();
    readonly paths = new Set<string>();

    /** No rules in any section (used for the additive no-op fast path). */
    isEmpty(): boolean {
        return this.tools.size === 0 && this.bashPatterns.size === 0 && this.networkHosts.size === 0 && this.paths.size === 0;
    }

    /** Parse from a TOML string. Missing sections default to empty. */
    static parse(tomlText: string): DenyRules {
        const raw = parseToml(tomlText) as DenyRulesToml;
        const r = new DenyRules();
        r.schemaVersion = typeof raw.schema_version === 'number' && raw.schema_version > 0 ? raw.schema_version : 1;
        for (const t of raw.tools?.deny ?? []) r.tools.add(t);
        for (const b of raw.bash?.deny_patterns ?? []) r.bashPatterns.add(b);
        for (const h of raw.network?.deny_hosts ?? []) r.networkHosts.add(h);
        for (const p of raw.paths?.deny ?? []) r.paths.add(p);
        return r;
    }

    /** Serialize to TOML. Empty sections omitted; entries sorted for a stable round-trip. */
    toTomlString(): string {
        const out: DenyRulesToml = { schema_version: this.schemaVersion };
        if (this.tools.size > 0) out.tools = { deny: [...this.tools].sort() };
        if (this.bashPatterns.size > 0) out.bash = { deny_patterns: [...this.bashPatterns].sort() };
        if (this.networkHosts.size > 0) out.network = { deny_hosts: [...this.networkHosts].sort() };
        if (this.paths.size > 0) out.paths = { deny: [...this.paths].sort() };
        return stringifyToml(out as Record<string, unknown>);
    }

    /** The first declarative rule this call matches, formatted as a deny reason, or `undefined`. */
    denyReason(call: ToolCall): string | undefined {
        // `[tools]` applies to ANY tool, whatever its category.
        for (const pat of this.tools) {
            if (globMatch(pat, call.name)) return `denied by policy (tools): ${pat}`;
        }
        const args = call.arguments;
        switch (toolCategory(call.name)) {
            case 'bash': {
                const cmd = strArg(args, ['cmd', 'command']).trim();
                if (cmd.length === 0) return undefined;
                const bashPat = this.bashDenied(cmd);
                if (bashPat !== undefined) return `denied by policy (bash): ${bashPat}`;
                // A denied host referenced by the command line is also blocked.
                for (const sub of splitCompound(cmd)) {
                    for (const host of extractHosts(sub)) {
                        const hostPat = this.hostDenied(host);
                        if (hostPat !== undefined) return `denied by policy (network): ${hostPat}`;
                    }
                }
                return undefined;
            }
            case 'network': {
                const raw = strArg(args, ['url', 'host']);
                const host = hostFromToken(raw) ?? raw;
                if (host.length === 0) return undefined;
                const pat = this.hostDenied(host);
                return pat !== undefined ? `denied by policy (network): ${pat}` : undefined;
            }
            case 'write':
            case 'safe': {
                for (const key of ['path', 'file', 'dir', 'directory']) {
                    const v = args[key];
                    if (typeof v === 'string') {
                        for (const pat of this.paths) {
                            if (globMatch(pat, v)) return `denied by policy (paths): ${pat}`;
                        }
                    }
                }
                return undefined;
            }
            case 'unknown':
                return undefined;
        }
    }

    /** First `[bash]` pattern that matches any (wrapper/sudo-stripped) subcommand. */
    private bashDenied(cmd: string): string | undefined {
        const subs = splitCompound(cmd).map((s) => stripWrappersAndSudo(s).toLowerCase());
        for (const pat of this.bashPatterns) {
            // A plain prefix (`"aws "`) gets an implicit trailing `*`; a pattern with
            // an explicit `*` also matches any trailing text so extra flags can't slip past.
            const lower = pat.toLowerCase();
            const anchored = lower.endsWith('*') ? lower : `${lower}*`;
            if (subs.some((sub) => globMatch(anchored, sub))) return pat;
        }
        return undefined;
    }

    /** First `[network]` pattern that matches `host` (case-insensitive). */
    private hostDenied(host: string): string | undefined {
        const h = host.toLowerCase();
        for (const pat of this.networkHosts) {
            if (hostPatternMatches(pat, h)) return pat;
        }
        return undefined;
    }
}

/** Pull a string argument by any of the given keys, or `''` when absent. */
function strArg(args: Record<string, unknown>, keys: readonly string[]): string {
    for (const k of keys) {
        const v = args[k];
        if (typeof v === 'string') return v;
    }
    return '';
}

/**
 * Match a single host deny pattern against an already-lowercased host.
 * - no `*` → subdomain-aware suffix match (`prod.internal` ⇒ `api.prod.internal`).
 * - `*.suffix` → apex + subdomains of `suffix`.
 * - mid-string `*` (`prod-*.rds.amazonaws.com`) → anchored glob.
 */
function hostPatternMatches(pattern: string, hostLower: string): boolean {
    const p = pattern.toLowerCase();
    if (!p.includes('*')) return domainMatchesSuffixList(hostLower, [p]);
    if (p.startsWith('*.')) {
        const bare = p.slice(2);
        if (domainMatchesSuffixList(hostLower, [bare])) return true;
    }
    return globMatch(p, hostLower);
}

/**
 * Minimal both-ends-anchored glob: `*` (and any run of `*`, so `**` too) matches
 * any sequence of characters, including `/`. No `?`, no char classes — deny globs
 * don't need them, and a tiny matcher stays auditable for a security-critical path.
 */
export function globMatch(pattern: string, text: string): boolean {
    const parts = pattern.split('*');
    if (parts.length === 1) return pattern === text; // no wildcard → exact match
    const first = parts[0];
    if (!text.startsWith(first)) return false;
    let pos = first.length;
    const lastIdx = parts.length - 1;
    for (let i = 1; i < parts.length; i++) {
        const part = parts[i];
        if (part.length === 0) continue; // consecutive / trailing `*`
        if (i === lastIdx) {
            // Last literal segment must sit at the very end, past the consumed region.
            const endStart = text.length - part.length;
            return endStart >= pos && text.endsWith(part);
        }
        const idx = text.indexOf(part, pos);
        if (idx < 0) return false;
        pos = idx + part.length;
    }
    // Pattern ended with `*` (last part empty): the trailing run matches anything.
    return true;
}

/**
 * A consumer-supplied semantic deny check. Runs on every gated tool call; a
 * returned reason is a hard deny (circuit-breaker tier). Use it for checks the
 * declarative rules can't express from strings alone — resolving an AWS call to
 * its account, a DB URL to writer-vs-replica, etc.
 *
 * The idiomatic TS seam: a function returning the deny reason, or `undefined` to
 * let the call fall through to the rest of the permission engine.
 */
export type DenyPredicate = (call: ToolCall) => string | undefined;

/**
 * Consumer-supplied deny policy: declarative rules + predicate checks. Attach to
 * the gate via {@link PermissionHook.withDenyPolicy}. An empty policy is a no-op.
 */
export class DenyPolicy {
    private readonly predicates: DenyPredicate[] = [];

    constructor(private declarative: DenyRules = new DenyRules()) {}

    /** Build the declarative half from a TOML string. Predicates are added via {@link withPredicate}. */
    static fromToml(tomlText: string): DenyPolicy {
        return new DenyPolicy(DenyRules.parse(tomlText));
    }

    /** Replace the declarative rules. Chainable. */
    withDeclarative(rules: DenyRules): this {
        this.declarative = rules;
        return this;
    }

    /** Add a consumer predicate. Chainable. */
    withPredicate(predicate: DenyPredicate): this {
        this.predicates.push(predicate);
        return this;
    }

    /** True when there are no rules and no predicates — nothing to deny. */
    isEmpty(): boolean {
        return this.declarative.isEmpty() && this.predicates.length === 0;
    }

    /**
     * The deny reason for `call`, or `undefined` to let it fall through. Declarative
     * rules are checked first, then predicates; the first match wins.
     */
    evaluate(call: ToolCall): string | undefined {
        const declarative = this.declarative.denyReason(call);
        if (declarative !== undefined) return declarative;
        for (const predicate of this.predicates) {
            const reason = predicate(call);
            if (reason !== undefined) return `denied by policy (predicate): ${reason}`;
        }
        return undefined;
    }
}
