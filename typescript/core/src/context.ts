/**
 * Project context loader — AGENTS.md (or its fallbacks) plus resolved file
 * references. The TypeScript port of the Rust reference
 * `smooth-operator-core::context` (pearl th-5002c4).
 *
 * Smooth previously only read AGENTS.md. Many projects don't have one but DO
 * have CLAUDE.md or a SMOOTH.md or .smooth/CONTEXT.md. User-level facts also
 * belong in the prompt, so we walk a preference order and STACK user-level +
 * project-level context.
 *
 * Preference order (first hit per layer; layers stack):
 *
 * - USER layer (read once, prepended):
 *   `~/.smooth/CONTEXT.md` → `~/.smooth/AGENTS.md` → `~/.smooth/CLAUDE.md`
 *
 * - PROJECT layer (walk up from `workingDir`, first hit wins):
 *   `<dir>/.smooth/CONTEXT.md` → `<dir>/SMOOTH.md` → `<dir>/AGENTS.md` → `<dir>/CLAUDE.md`
 *
 * AGENTS.md / SMOOTH.md can carry file references in a `## File References`
 * section:
 *
 * ```markdown
 * ## File References
 * - [CLAUDE.md](CLAUDE.md) — full file
 * - [Section name](CLAUDE.md#6-pearl-tracking) — specific section
 * ```
 *
 * Those are resolved against the file's directory and appended inline. The
 * combined string is what the host injects into the agent's system prompt —
 * like the Rust reference, this is a standalone loader, NOT a hook inside the
 * agent loop.
 */

import { readFileSync, statSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';

/** A parsed file reference from the `## File References` section. */
export interface FileRef {
    /** Display label from the markdown link text. */
    label: string;
    /** Relative file path (without fragment). */
    path: string;
    /** Optional `#fragment` pointing to a heading. */
    fragment?: string;
    /** Optional description after the ` — `. */
    description?: string;
}

const USER_CONTEXT_CANDIDATES = ['.smooth/CONTEXT.md', '.smooth/AGENTS.md', '.smooth/CLAUDE.md'];
const PROJECT_CONTEXT_CANDIDATES = ['.smooth/CONTEXT.md', 'SMOOTH.md', 'AGENTS.md', 'CLAUDE.md'];

/**
 * Load the combined project + user context, user-level prepended, with file
 * references in any AGENTS.md / SMOOTH.md resolved inline.
 *
 * `undefined` only when NEITHER layer found anything — so a workspace with a
 * bare CLAUDE.md and no user-level file still loads context.
 */
export function loadProjectContext(workingDir: string): string | undefined {
    const user = loadUserContext();
    const project = loadLayeredProjectContext(workingDir);

    if (user === undefined && project === undefined) return undefined;
    if (project === undefined) return `## User context (~/.smooth)\n\n${user}`;
    if (user === undefined) return project;
    return `## User context (~/.smooth)\n\n${user}\n\n---\n\n${project}`;
}

/** First non-blank hit from the user-level preference list. */
function loadUserContext(): string | undefined {
    const home = homedir();
    if (!home) return undefined;
    for (const candidate of USER_CONTEXT_CANDIDATES) {
        const raw = readFileOrUndefined(join(home, candidate));
        if (raw !== undefined && raw.trim() !== '') return raw;
    }
    return undefined;
}

/** The project context file with its file references resolved. */
function loadLayeredProjectContext(workingDir: string): string | undefined {
    const contextPath = findProjectContextFile(workingDir);
    if (contextPath === undefined) return undefined;
    const raw = readFileOrUndefined(contextPath);
    if (raw === undefined) return undefined;

    const refs = parseFileReferences(raw);
    if (refs.length === 0) return raw;

    const resolved = resolveReferences(dirname(contextPath), refs);
    if (resolved.length === 0) return raw;

    let output = raw;
    output += '\n---\n\n## Resolved File References\n\n';
    for (const { ref, content } of resolved) {
        output += ref.description === undefined ? `### ${ref.label}\n` : `### ${ref.label} — ${ref.description}\n`;
        output += '\n```\n';
        output += content;
        if (!content.endsWith('\n')) output += '\n';
        output += '```\n\n';
    }
    return output;
}

/**
 * Walk up from `startDir` looking for a project context file. Preference order
 * at each level: `.smooth/CONTEXT.md` → `SMOOTH.md` → `AGENTS.md` →
 * `CLAUDE.md`. First hit wins per directory, then keep walking up.
 */
export function findProjectContextFile(startDir: string): string | undefined {
    let dir = startDir;
    for (;;) {
        for (const candidate of PROJECT_CONTEXT_CANDIDATES) {
            const path = join(dir, candidate);
            try {
                if (statSync(path).isFile()) return path;
            } catch {
                // Not there — try the next candidate.
            }
        }
        const parent = dirname(dir);
        if (parent === dir) return undefined;
        dir = parent;
    }
}

/**
 * Parse the `## File References` section out of AGENTS.md content. Expects
 * markdown list items like `- [Label](path.md#fragment) — description`.
 */
export function parseFileReferences(content: string): FileRef[] {
    const refs: FileRef[] = [];
    let inSection = false;

    for (const line of splitLines(content)) {
        const trimmed = line.trim();

        // Detect the file references section.
        if (trimmed.startsWith('## ') || trimmed.startsWith('# ')) {
            inSection = trimmed.toLowerCase().includes('file reference');
            continue;
        }
        if (!inSection) continue;

        const ref = parseLinkLine(trimmed);
        if (ref !== undefined) refs.push(ref);
    }
    return refs;
}

/** Parse a single markdown list-item link line. */
export function parseLinkLine(line: string): FileRef | undefined {
    // Strip leading `- ` or `* `.
    let rest: string;
    if (line.startsWith('- ')) rest = line.slice(2);
    else if (line.startsWith('* ')) rest = line.slice(2);
    else return undefined;

    // Match [label](target).
    const openBracket = rest.indexOf('[');
    if (openBracket < 0) return undefined;
    const closeBracket = rest.indexOf(']', openBracket);
    if (closeBracket < 0) return undefined;
    const label = rest.slice(openBracket + 1, closeBracket);

    const after = rest.slice(closeBracket + 1);
    const openParen = after.indexOf('(');
    if (openParen < 0) return undefined;
    const closeParen = after.indexOf(')', openParen);
    if (closeParen < 0) return undefined;
    const target = after.slice(openParen + 1, closeParen);

    // Split path and fragment.
    const hash = target.indexOf('#');
    const path = hash < 0 ? target : target.slice(0, hash);
    const fragment = hash < 0 ? undefined : target.slice(hash + 1);

    // Description after ` — ` / ` - ` / ` -- `.
    const afterLink = after.slice(closeParen + 1);
    let description: string | undefined;
    for (const sep of [' — ', ' - ', ' -- ']) {
        if (afterLink.startsWith(sep)) {
            const d = afterLink.slice(sep.length).trim();
            if (d !== '') description = d;
            break;
        }
    }

    if (path === '' && fragment === undefined) return undefined;
    return { label, path, fragment, description };
}

/** Resolve file references against a base directory, skipping unreadable ones. */
function resolveReferences(baseDir: string, refs: FileRef[]): { ref: FileRef; content: string }[] {
    const results: { ref: FileRef; content: string }[] = [];
    for (const ref of refs) {
        const raw = readFileOrUndefined(join(baseDir, ref.path));
        if (raw === undefined) continue; // Skip unreadable files.
        const content = ref.fragment === undefined ? raw : extractSection(raw, ref.fragment);
        if (content.trim() !== '') results.push({ ref, content });
    }
    return results;
}

/**
 * Extract a markdown section by heading fragment. The fragment is matched
 * against GitHub-style heading anchors; the section runs until the next
 * heading at the same or a higher level.
 */
export function extractSection(content: string, fragment: string): string {
    const target = headingToAnchor(fragment);
    const lines = splitLines(content);
    let start = -1;
    let startLevel = 0;

    for (let i = 0; i < lines.length; i++) {
        const heading = parseHeading(lines[i]!);
        if (heading === undefined) continue;
        const anchor = headingToAnchor(heading.text);
        if (anchor === target || anchor.includes(target) || target.includes(anchor)) {
            start = i;
            startLevel = heading.level;
            continue;
        }
        // Started capturing and hit a same-or-higher-level heading: stop.
        if (start >= 0 && heading.level <= startLevel) return lines.slice(start, i).join('\n');
    }

    return start >= 0 ? lines.slice(start).join('\n') : '';
}

/** Parse a markdown heading line into its level and text. */
function parseHeading(line: string): { level: number; text: string } | undefined {
    const trimmed = line.trim();
    if (!trimmed.startsWith('#')) return undefined;
    const level = trimmed.length - trimmed.replace(/^#+/, '').length;
    const text = trimmed.slice(level).trim();
    if (text === '') return undefined;
    return { level, text };
}

/** Convert heading text to a GitHub-style anchor. */
export function headingToAnchor(text: string): string {
    let out = '';
    for (const ch of text.toLowerCase()) {
        if (/[\p{L}\p{N}\-_]/u.test(ch)) out += ch;
        else if (ch === ' ') out += '-';
        // Other characters are dropped.
    }
    // Single non-overlapping pass, matching Rust's `str::replace`.
    return out.replaceAll('--', '-');
}

/** `undefined` instead of throwing, for any unreadable path. */
function readFileOrUndefined(path: string): string | undefined {
    try {
        return readFileSync(path, 'utf8');
    } catch {
        return undefined;
    }
}

/**
 * Mirrors Rust's `str::lines`: split on \n, drop a trailing \r on each line,
 * and emit no trailing empty line for a final newline.
 */
function splitLines(s: string): string[] {
    if (s === '') return [];
    const body = s.endsWith('\n') ? s.slice(0, -1) : s;
    return body.split('\n').map((line) => (line.endsWith('\r') ? line.slice(0, -1) : line));
}
