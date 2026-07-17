/**
 * Tests for the consumer deny policy — ports the Rust engine's `deny_policy.rs`
 * suite: the tiny glob matcher, each declarative section (tools/bash/network/
 * paths) with adversarial sudo/compound/wrapper bash inputs, the predicate tier,
 * the empty no-op, and the TOML round-trip.
 */

import { describe, expect, it } from 'vitest';
import { DenyPolicy, DenyRules, globMatch } from '../src/denyPolicy.js';
import { ToolCall } from '../src/permission.js';

const call = (name: string, args: Record<string, unknown>): ToolCall => ({ id: 'c1', name, arguments: args });
const bashCall = (cmd: string): ToolCall => call('bash', { cmd });

// ── glob matcher ───────────────────────────────────────────────
describe('globMatch', () => {
    it('handles exact and wildcards including / spanning', () => {
        expect(globMatch('exact', 'exact')).toBe(true);
        expect(globMatch('exact', 'exacts')).toBe(false);
        expect(globMatch('vendor.*', 'vendor.delete')).toBe(true);
        expect(globMatch('vendor.*', 'other.delete')).toBe(false);
        expect(globMatch('*.delete', 'vendor.delete')).toBe(true);
        expect(globMatch('*.delete', 'vendor.deleted')).toBe(false);
        expect(globMatch('a*c', 'abc')).toBe(true);
        expect(globMatch('a*c', 'ac')).toBe(true);
        expect(globMatch('a*c', 'ab')).toBe(false);
        expect(globMatch('/prod/**', '/prod/secrets/db.txt')).toBe(true);
        expect(globMatch('/prod/**', '/staging/x')).toBe(false);
        expect(globMatch('**/secrets/**', '/a/b/secrets/c/d')).toBe(true);
        expect(globMatch('**/secrets/**', '/a/b/c')).toBe(false);
    });
});

// ── declarative sections ───────────────────────────────────────
describe('declarative rules', () => {
    it('tools section denies matches, allows non-matches', () => {
        const policy = DenyPolicy.fromToml(`
            [tools]
            deny = ["vendor.dangerous_tool", "*.delete_prod"]
        `);
        expect(policy.evaluate(call('vendor.dangerous_tool', {}))).toBeDefined();
        expect(policy.evaluate(call('svc.delete_prod', {}))).toBeDefined();
        expect(policy.evaluate(call('vendor.safe_tool', {}))).toBeUndefined();
    });

    it('bash section denies matches, allows non-matches', () => {
        const policy = DenyPolicy.fromToml(`
            [bash]
            deny_patterns = ["aws * --profile prod", "terraform apply"]
        `);
        expect(policy.evaluate(bashCall('aws s3 ls --profile prod'))).toBeDefined();
        expect(policy.evaluate(bashCall('terraform apply -auto-approve'))).toBeDefined();
        expect(policy.evaluate(bashCall('aws s3 ls --profile dev'))).toBeUndefined();
        expect(policy.evaluate(bashCall('aws s3 ls'))).toBeUndefined();
    });

    it('bash prefix respects the trailing-space word boundary', () => {
        const policy = DenyPolicy.fromToml(`
            [bash]
            deny_patterns = ["aws "]
        `);
        expect(policy.evaluate(bashCall('aws s3 ls'))).toBeDefined();
        expect(policy.evaluate(bashCall('awslocal s3 ls'))).toBeUndefined();
    });

    it('bash deny survives sudo, compound, wrappers, and extra flags (adversarial)', () => {
        const policy = DenyPolicy.fromToml(`
            [bash]
            deny_patterns = ["aws * --profile prod"]
        `);
        expect(policy.evaluate(bashCall('sudo aws s3 rm s3://b --profile prod'))).toBeDefined();
        expect(policy.evaluate(bashCall('ls && aws s3 ls --profile prod'))).toBeDefined();
        expect(policy.evaluate(bashCall('aws s3 ls --profile prod --region us-east-1'))).toBeDefined();
        expect(policy.evaluate(bashCall('timeout 5 aws s3 ls --profile prod'))).toBeDefined();
    });

    it('network section denies suffix + glob patterns, and a curl in bash', () => {
        const policy = DenyPolicy.fromToml(`
            [network]
            deny_hosts = ["*.prod.internal", "prod-*.rds.amazonaws.com", "secrets.example.com"]
        `);
        expect(policy.evaluate(call('web_fetch', { url: 'https://api.prod.internal/x' }))).toBeDefined();
        expect(policy.evaluate(call('web_fetch', { url: 'https://prod.internal/' }))).toBeDefined();
        expect(policy.evaluate(call('web_fetch', { url: 'https://prod-db1.rds.amazonaws.com' }))).toBeDefined();
        expect(policy.evaluate(call('web_fetch', { host: 'api.secrets.example.com' }))).toBeDefined();
        expect(policy.evaluate(call('web_fetch', { url: 'https://staging.internal/x' }))).toBeUndefined();
        expect(policy.evaluate(bashCall('curl https://api.prod.internal/health'))).toBeDefined();
    });

    it('paths section denies write and read tools', () => {
        const policy = DenyPolicy.fromToml(`
            [paths]
            deny = ["/prod/**", "**/secrets/**"]
        `);
        expect(policy.evaluate(call('file_write', { path: '/prod/config.yaml' }))).toBeDefined();
        expect(policy.evaluate(call('read_file', { path: '/app/secrets/db.env' }))).toBeDefined();
        expect(policy.evaluate(call('list_dir', { dir: '/prod/data' }))).toBeDefined();
        expect(policy.evaluate(call('file_write', { path: '/app/src/main.rs' }))).toBeUndefined();
    });
});

// ── predicate tier ─────────────────────────────────────────────
describe('predicate tier', () => {
    it('some denies, none falls through', () => {
        const policy = new DenyPolicy().withPredicate((c) =>
            String((c.arguments.cmd as string) ?? '').includes('999999999999') ? 'resolved to the prod AWS account' : undefined,
        );
        expect(policy.evaluate(bashCall('aws s3 ls --profile acct-999999999999'))).toContain('prod AWS account');
        expect(policy.evaluate(bashCall('aws s3 ls --profile acct-111'))).toBeUndefined();
    });

    it('declarative reason wins over a predicate', () => {
        const policy = DenyPolicy.fromToml(`
            [tools]
            deny = ["vendor.tool"]
        `).withPredicate(() => 'predicate always denies');
        expect(policy.evaluate(call('vendor.tool', {}))).toContain('(tools)');
        expect(policy.evaluate(call('other.tool', {}))).toContain('(predicate)');
    });
});

// ── empty no-op + TOML round-trip ──────────────────────────────
describe('empty policy and round-trip', () => {
    it('an empty policy denies nothing', () => {
        const policy = new DenyPolicy();
        expect(policy.isEmpty()).toBe(true);
        expect(policy.evaluate(bashCall('rm -rf /prod'))).toBeUndefined();
        expect(policy.evaluate(call('file_write', { path: '/prod/x' }))).toBeUndefined();
        expect(policy.evaluate(call('vendor.anything', {}))).toBeUndefined();
    });

    it('DenyRules survive a TOML round-trip', () => {
        const rules = new DenyRules();
        rules.tools.add('vendor.dangerous_tool');
        rules.bashPatterns.add('aws * --profile prod');
        rules.networkHosts.add('*.prod.internal');
        rules.paths.add('/prod/**');
        const text = rules.toTomlString();
        const reparsed = DenyRules.parse(text);
        expect([...reparsed.tools]).toEqual([...rules.tools]);
        expect([...reparsed.bashPatterns]).toEqual([...rules.bashPatterns]);
        expect([...reparsed.networkHosts]).toEqual([...rules.networkHosts]);
        expect([...reparsed.paths]).toEqual([...rules.paths]);
    });

    it('empty rules parse and are empty', () => {
        expect(DenyRules.parse('').isEmpty()).toBe(true);
        expect(DenyRules.parse('schema_version = 1').isEmpty()).toBe(true);
    });
});
