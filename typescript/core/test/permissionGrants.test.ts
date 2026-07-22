/**
 * Tests for the permission allow-list — ports the Rust engine's
 * `permission_grants.rs` matching + merge + TOML round-trip coverage.
 */

import { describe, expect, it } from 'vitest';
import { hostMatchesGlob, PermissionGrants } from '../src/permissionGrants.js';

describe('host glob matching', () => {
    it('exact host and *.wildcard (apex + subdomains), no substring slip', () => {
        const g = new PermissionGrants();
        g.add({ kind: 'network', host: 'api.example.com' });
        expect(g.matchesHost('api.example.com')).toBe(true);
        expect(g.matchesHost('API.EXAMPLE.COM')).toBe(true);
        expect(g.matchesHost('other.example.com')).toBe(false);

        const w = new PermissionGrants();
        w.add({ kind: 'network', host: '*.example.com' });
        expect(w.matchesHost('api.example.com')).toBe(true);
        expect(w.matchesHost('example.com')).toBe(true); // bare apex
        expect(w.matchesHost('evil-example.com')).toBe(false);
    });

    it('bare host requires an exact match', () => {
        const g = new PermissionGrants();
        g.add({ kind: 'network', host: 'example.com' });
        expect(g.matchesHost('example.com')).toBe(true);
        expect(g.matchesHost('api.example.com')).toBe(false);
        expect(g.matchesHost('evil-example.com')).toBe(false);
    });

    it('hostMatchesGlob standalone', () => {
        expect(hostMatchesGlob('api.example.com', '*.example.com')).toBe(true);
        expect(hostMatchesGlob('evil-example.com', 'example.com')).toBe(false);
    });
});

describe('tool + bash matching', () => {
    it('tool match is exact only', () => {
        const g = new PermissionGrants();
        g.add({ kind: 'tool', tool: 'web_search' });
        expect(g.matchesTool('web_search')).toBe(true);
        expect(g.matchesTool('web_search_v2')).toBe(false);
    });

    it('bash prefix respects the trailing-space guard', () => {
        const g = new PermissionGrants();
        g.add({ kind: 'bash', prefix: 'cargo ' });
        expect(g.matchesBash('cargo test')).toBe(true);
        expect(g.matchesBash('CARGO BUILD')).toBe(true);
        expect(g.matchesBash('cargonaut')).toBe(false);
    });

    it('contains matches add', () => {
        const g = new PermissionGrants();
        const q = { kind: 'bash', prefix: 'npm ' } as const;
        expect(g.contains(q)).toBe(false);
        g.add(q);
        expect(g.contains(q)).toBe(true);
    });
});

describe('merge and round-trip', () => {
    it('merge unions all sections', () => {
        const a = new PermissionGrants();
        a.add({ kind: 'network', host: 'a.example.com' });
        const b = new PermissionGrants();
        b.add({ kind: 'tool', tool: 't' });
        b.add({ kind: 'bash', prefix: 'pnpm ' });
        a.mergeWith(b);
        expect(a.matchesHost('a.example.com')).toBe(true);
        expect(a.matchesTool('t')).toBe(true);
        expect(a.matchesBash('pnpm i')).toBe(true);
    });

    it('survives a TOML round-trip (config round-trip)', () => {
        const g = new PermissionGrants();
        g.add({ kind: 'network', host: '*.openai.com' });
        g.add({ kind: 'tool', tool: 'web_search' });
        g.add({ kind: 'bash', prefix: 'cargo ' });
        const reparsed = PermissionGrants.parse(g.toTomlString());
        expect([...reparsed.allowHosts]).toEqual([...g.allowHosts]);
        expect([...reparsed.allowTools]).toEqual([...g.allowTools]);
        expect([...reparsed.allowBashPatterns]).toEqual([...g.allowBashPatterns]);
        expect(reparsed.schemaVersion).toBe(1);
    });

    it('empty TOML parses to an empty grant set at schema 1', () => {
        const g = PermissionGrants.parse('');
        expect(g.schemaVersion).toBe(1);
        expect(g.allowHosts.size).toBe(0);
    });
});
