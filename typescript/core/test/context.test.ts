// Ports the Rust reference engine's `context.rs` tests one-for-one.

import { mkdtempSync, mkdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { extractSection, findProjectContextFile, headingToAnchor, loadProjectContext, parseFileReferences, parseLinkLine } from '../src/context.js';

function tempDir(): string {
    return mkdtempSync(join(tmpdir(), 'smooth-ctx-'));
}

function write(path: string, content: string): void {
    mkdirSync(dirname(path), { recursive: true });
    writeFileSync(path, content, 'utf8');
}

describe('parseLinkLine', () => {
    it('parses a simple link', () => {
        const r = parseLinkLine('- [CLAUDE.md](CLAUDE.md) — Project overview');
        expect(r).toEqual({ label: 'CLAUDE.md', path: 'CLAUDE.md', fragment: undefined, description: 'Project overview' });
    });

    it('parses a link with a fragment', () => {
        const r = parseLinkLine('- [Pearl tracking](CLAUDE.md#6-pearl-tracking) — Pearl workflow');
        expect(r).toEqual({ label: 'Pearl tracking', path: 'CLAUDE.md', fragment: '6-pearl-tracking', description: 'Pearl workflow' });
    });

    it('parses a link with no description', () => {
        const r = parseLinkLine('- [README](README.md)');
        expect(r).toEqual({ label: 'README', path: 'README.md', fragment: undefined, description: undefined });
    });
});

describe('parseFileReferences', () => {
    it('only collects links inside the File References section', () => {
        const content =
            '# Agent Instructions\n\nSome intro text.\n\n## File References\n\n' +
            '- [CLAUDE.md](CLAUDE.md) — Full file\n' +
            '- [Testing](CLAUDE.md#8-testing) — Testing reqs\n\n' +
            '## Other Section\n\n' +
            '- [not a ref](foo.md)\n';

        const refs = parseFileReferences(content);
        expect(refs).toHaveLength(2);
        expect(refs[0]!.path).toBe('CLAUDE.md');
        expect(refs[0]!.fragment).toBeUndefined();
        expect(refs[1]!.path).toBe('CLAUDE.md');
        expect(refs[1]!.fragment).toBe('8-testing');
    });
});

describe('headingToAnchor', () => {
    it('makes GitHub-style anchors', () => {
        expect(headingToAnchor('6. Pearl Tracking')).toBe('6-pearl-tracking');
        expect(headingToAnchor('Testing - MANDATORY')).toBe('testing--mandatory');
        expect(headingToAnchor('Simple Heading')).toBe('simple-heading');
    });
});

describe('extractSection', () => {
    it('extracts a section by fragment and stops at the next same-level heading', () => {
        const content = '# Top\n\nIntro\n\n## Section A\n\nContent A\n\n## Section B\n\nContent B\n\n### Subsection\n\nSub content\n';
        const section = extractSection(content, 'section-a');
        expect(section).toContain('## Section A');
        expect(section).toContain('Content A');
        expect(section).not.toContain('Section B');
    });

    it('runs to EOF when there is no following heading', () => {
        const section = extractSection('# Top\n\n## Last Section\n\nFinal content\n', 'last-section');
        expect(section).toContain('## Last Section');
        expect(section).toContain('Final content');
    });

    it('returns empty when the fragment is not found', () => {
        expect(extractSection('# Top\n\n## Existing\n\nContent\n', 'nonexistent')).toBe('');
    });
});

describe('loadProjectContext', () => {
    it('loads AGENTS.md and inlines its resolved file references', () => {
        const tmp = tempDir();
        write(join(tmp, 'CLAUDE.md'), '# Project\n\nOverview\n\n## Testing\n\nAll tests must pass.\n\n## Deploy\n\nNever deploy locally.\n');
        write(join(tmp, 'AGENTS.md'), '# Agent Instructions\n\n## File References\n\n- [Testing](CLAUDE.md#testing) — Test reqs\n\n## Rules\n\nBe helpful.\n');

        const ctx = loadProjectContext(tmp);
        expect(ctx).toBeDefined();
        expect(ctx).toContain('Agent Instructions');
        expect(ctx).toContain('Resolved File References');
        expect(ctx).toContain('All tests must pass');
    });

    it('loads nothing referring to a dir with no context files', () => {
        // Mirrors the Rust test's caveat: the walk-up escapes to the filesystem
        // root and the user layer can't be mocked, so assert the practical
        // guarantee — nothing loaded refers to the (empty) temp dir.
        const tmp = tempDir();
        const ctx = loadProjectContext(tmp);
        if (ctx !== undefined) expect(ctx).not.toContain(tmp);
    });

    it('prefers .smooth/CONTEXT.md over CLAUDE.md', () => {
        const tmp = tempDir();
        write(join(tmp, '.smooth/CONTEXT.md'), '# Smooth context\n\nthe winner');
        write(join(tmp, 'CLAUDE.md'), '# Claude.md\n\nshould lose');

        const ctx = loadProjectContext(tmp);
        expect(ctx).toContain('the winner');
        expect(ctx).not.toContain('should lose');
    });

    it('falls back to CLAUDE.md when there is no AGENTS.md', () => {
        const tmp = tempDir();
        write(join(tmp, 'CLAUDE.md'), '# CLAUDE.md\n\nfallback content');
        expect(loadProjectContext(tmp)).toContain('fallback content');
    });

    it('prefers SMOOTH.md over CLAUDE.md', () => {
        const tmp = tempDir();
        write(join(tmp, 'SMOOTH.md'), '# SMOOTH.md\n\nsmooth wins');
        write(join(tmp, 'CLAUDE.md'), '# CLAUDE.md\n\nclaude loses');

        const ctx = loadProjectContext(tmp);
        expect(ctx).toContain('smooth wins');
        expect(ctx).not.toContain('claude loses');
    });

    it('returns the raw file verbatim when there are no file references', () => {
        const tmp = tempDir();
        write(join(tmp, 'AGENTS.md'), '# Agent Instructions\n\nJust some text.\n');
        expect(loadProjectContext(tmp)).toBe('# Agent Instructions\n\nJust some text.\n');
    });
});

describe('findProjectContextFile', () => {
    it('walks up to a parent directory', () => {
        const tmp = tempDir();
        const nested = join(tmp, 'a/b/c');
        mkdirSync(nested, { recursive: true });
        write(join(tmp, 'CLAUDE.md'), '# CLAUDE.md at root');

        expect(findProjectContextFile(nested)?.endsWith('CLAUDE.md')).toBe(true);
    });
});
