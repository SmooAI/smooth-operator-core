/**
 * Native secret-detection + prompt-injection scanning {@link ToolHook} — the
 * TypeScript port of the Rust reference engine's `narc.rs` (pearl th-5f7227).
 *
 * The SEP extension host passes tool-call arguments to the extension subprocess
 * **unscanned** and returns the subprocess's tool-result content to the model
 * **verbatim**. Nothing at the extension boundary looks for leaked credentials or
 * prompt-injection payloads. {@link NarcHook} closes that gap. It scans two things:
 *
 * - **Secrets** — 10 credential patterns (AWS keys, private keys, JWTs/bearer
 *   tokens, high-entropy provider keys, …).
 * - **Prompt injection** — 8 patterns (instruction override, role hijack,
 *   jailbreak, data/URL exfiltration, …).
 *
 * ## Division of labour with `PermissionHook`
 *
 * The permission gate already owns the *dangerous-command* / *write* /
 * *credential-path* circuit-breakers (`rm -rf /`, `curl | sh`, `~/.ssh/id_rsa`).
 * Narc does **not** re-implement those — it is scoped to the one thing permission
 * does not do: **content scanning of arguments and results** for secrets and
 * injection. Install Narc *after* the permission hook so the allow/ask/deny
 * decision happens first and Narc scans the calls that clear it.
 *
 * ## `preCall` (arguments) — blocks on exfiltration, alerts otherwise
 *
 * A {@link Severity.Block} injection match (the active data/URL exfiltration
 * signals) throws, blocking the call before the tool runs. Lower-severity
 * injection and any secret in the arguments are **alerted, not blocked** — a tool
 * argument legitimately carrying a secret (writing a `.env`, configuring a
 * client) is common enough that a hard block there would be a footgun.
 *
 * ## `postCall` (result) — detects, alerts, and **redacts** secrets
 *
 * `postCall` receives a **mutable** result, so a rewrite of `result.content` is
 * what downstream consumers — and the LLM/conversation — actually see. A secret
 * pattern in a tool result raises a {@link Severity.Block} alert *and* replaces
 * the matched credential with `[REDACTED:<pattern-name>]` before it reaches the
 * model. Injection patterns in the result remain detection + {@link Severity.Alert}
 * only (surveillance) — they can appear in legitimate content and are not rewritten.
 *
 * The detection set is pinned across all five engines by the shared corpus at
 * `spec/narc/corpus.json` (see `test/narc.test.ts`).
 */

import type { ToolCall, ToolHook, ToolResult } from './agent.js';

/**
 * Severity of a Narc finding, ordered least → most severe. A
 * {@link Severity.Block} finding in `preCall` blocks the tool call. The numeric
 * values ARE the ordering — compare with `>=`.
 */
export enum Severity {
    /** Informational — no action. */
    Info = 0,
    /** Suspicious but plausibly legitimate (e.g. a secret in an argument). */
    Warn = 1,
    /** Strong signal worth surfacing, but not auto-blocked. */
    Alert = 2,
    /** Actively harmful — blocks the call when raised in `preCall`. */
    Block = 3,
}

/** The shared wire label for a severity (`INFO`/`WARN`/`ALERT`/`BLOCK`). */
export function severityLabel(s: Severity): string {
    switch (s) {
        case Severity.Warn:
            return 'WARN';
        case Severity.Alert:
            return 'ALERT';
        case Severity.Block:
            return 'BLOCK';
        default:
            return 'INFO';
    }
}

/**
 * A single surveillance finding. Lean by design — the consumer supplies the
 * timestamp and correlation, so no uuid/timestamp fields are carried.
 */
export interface NarcAlert {
    /** How severe the finding is. */
    severity: Severity;
    /** Coarse bucket: `'injection'`, `'secret'`, `'secret_leak'`, `'injection_output'`. */
    category: string;
    /** The named pattern that matched. */
    patternName: string;
    /** Redacted view of the matched text (never the raw secret). */
    redacted: string;
    /** The tool whose args/result triggered the finding. */
    toolName: string;
}

/** A pattern match: which pattern, its severity, and a redacted view. */
export interface NarcFinding {
    /** The named pattern that matched. */
    patternName: string;
    /** The finding's severity. */
    severity: Severity;
    /** Redacted view of the matched text (safe to log). */
    redacted: string;
}

interface NamedPattern {
    name: string;
    severity: Severity;
    regex: RegExp;
}

/**
 * The 10 secret patterns. All are {@link Severity.Warn} in arguments (may be
 * legit) and escalate to {@link Severity.Block} when found in a result (a
 * leak) — the caller decides which threshold to apply.
 *
 * Every regex carries `g` because the scanners enumerate **all** matches. That
 * makes `RegExp.test` stateful on these objects, so this module never calls it —
 * `hasSecrets`/`hasInjection` go through the scanners.
 */
const SECRET_PATTERNS: NamedPattern[] = [
    { name: 'AWS Access Key', severity: Severity.Warn, regex: /AKIA[0-9A-Z]{16}/g },
    { name: 'AWS Secret Key', severity: Severity.Warn, regex: /aws[_\-]?secret[_\-]?access[_\-]?key\s*[=:]\s*[A-Za-z0-9/+=]{40}/gi },
    { name: 'Anthropic API Key', severity: Severity.Warn, regex: /sk-ant-[A-Za-z0-9\-_]{20,}/g },
    { name: 'OpenAI API Key', severity: Severity.Warn, regex: /sk-[A-Za-z0-9]{20,}/g },
    { name: 'GitHub Token', severity: Severity.Warn, regex: /gh[posr]_[A-Za-z0-9_]{36,}/g },
    { name: 'Private Key', severity: Severity.Warn, regex: /-----BEGIN\s+(RSA\s+)?PRIVATE\s+KEY-----/g },
    { name: 'Generic Secret', severity: Severity.Warn, regex: /(secret|password|token|api[_\-]?key)\s*[=:]\s*["']?[A-Za-z0-9/+=\-_]{8,}/gi },
    { name: 'Bearer Token', severity: Severity.Warn, regex: /Bearer\s+[A-Za-z0-9\-_.~+/]+=*/g },
    { name: 'Base64 Encoded Key', severity: Severity.Warn, regex: /(key|secret|password)\s*[=:]\s*[A-Za-z0-9+/]{32,}={0,2}/gi },
    { name: 'Stripe Key', severity: Severity.Warn, regex: /[sr]k_(live|test)_[A-Za-z0-9]{20,}/g },
];

/**
 * The 8 injection patterns. Only the active data/URL exfiltration signals are
 * {@link Severity.Block} (blocked in arguments); hijack/jailbreak text is
 * {@link Severity.Alert} (surveilled, not blocked — it can appear in legitimate
 * content the model is authoring, e.g. a security test or documentation about
 * injection).
 *
 * `data_exfiltration` is the Rust pattern's free-spacing `(?x)` form flattened to
 * one line: JavaScript has no `x` flag, and a rewrite that dropped an alternative
 * would silently weaken the detector.
 */
const INJECTION_PATTERNS: NamedPattern[] = [
    { name: 'ignore_instructions', severity: Severity.Alert, regex: /ignore\s+(all\s+)?(previous|prior|above)\s+(instructions|prompts|rules)/gi },
    { name: 'role_hijack', severity: Severity.Alert, regex: /(you\s+are\s+now|act\s+as|pretend\s+(to\s+be|you\s+are)|from\s+now\s+on\s+you\s+are)/gi },
    { name: 'system_prompt', severity: Severity.Alert, regex: /(system\s*:\s*|<\|system\|>|\[SYSTEM\])/gi },
    { name: 'jailbreak', severity: Severity.Alert, regex: /(DAN\s+mode|developer\s+mode|do\s+anything\s+now|jailbreak)/gi },
    { name: 'base64_smuggling', severity: Severity.Alert, regex: /(decode|eval|execute)\s+(this\s+)?(base64|encoded)/gi },
    {
        name: 'data_exfiltration',
        severity: Severity.Block,
        regex: /(send|post|upload|exfiltrate|transmit|leak|push)\s+(all\s+|the\s+|our\s+|my\s+|this\s+)*(data|files?|secrets?|credentials?|keys?|tokens?|contents?|env\s+(vars?|file)|package\.json|\.env|pyproject\.toml|cargo\.toml|requirements\.txt|gemfile|go\.mod|composer\.json|\.ssh\/[a-z_]+|id_rsa|\.aws\/[a-z]+|\.gnupg\/)\s+(to|via|at|over)/gi,
    },
    {
        name: 'url_exfiltration',
        severity: Severity.Block,
        regex: /(send|post|upload|push|transmit|leak|exfiltrate)\b[^.\n]{1,200}\s+(to|via|at|over)\s+(https?:\/\/[\w.\-/]+)/gi,
    },
    { name: 'smell_url', severity: Severity.Alert, regex: /https?:\/\/[\w.\-]*\b(leak|exfil|attacker|evil|tracker|c2(?:server)?|webhook\.site)\b[\w.\-/]*/gi },
];

function scan(patterns: NamedPattern[], text: string): NarcFinding[] {
    const out: NarcFinding[] = [];
    for (const p of patterns) {
        for (const m of text.matchAll(p.regex)) {
            out.push({ patternName: p.name, severity: p.severity, redacted: redactMatch(m[0]) });
        }
    }
    return out;
}

/** Scan `text` for hardcoded secrets. Every match is redacted. */
export function scanSecrets(text: string): NarcFinding[] {
    return scan(SECRET_PATTERNS, text);
}

/** Scan `text` for prompt-injection patterns. Matched text is redacted. */
export function scanInjection(text: string): NarcFinding[] {
    return scan(INJECTION_PATTERNS, text);
}

/** True if `text` contains any secret pattern. */
export function hasSecrets(text: string): boolean {
    return scanSecrets(text).length > 0;
}

/** True if `text` contains any injection pattern. */
export function hasInjection(text: string): boolean {
    return scanInjection(text).length > 0;
}

/**
 * Redact a matched string, showing only the first 4 and last 2 characters. Short
 * matches (≤ 8 code points) are fully starred. Code points, not UTF-16 units —
 * parity with the Rust reference's char-based redaction.
 */
export function redactMatch(s: string): string {
    const chars = [...s];
    if (chars.length <= 8) return '*'.repeat(chars.length);
    return chars.slice(0, 4).join('') + '*'.repeat(chars.length - 6) + '**' + chars.slice(chars.length - 2).join('');
}

/**
 * A {@link ToolHook} that scans tool-call arguments and results for secrets and
 * prompt injection. Install it via `AgentOptions.toolHooks` alongside the
 * permission hook, *after* it.
 *
 * - **`preCall`** throws on a {@link Severity.Block} injection pattern in the
 *   arguments (active exfiltration); every other finding (lower-severity
 *   injection, any secret) is recorded as a {@link NarcAlert}, not blocked.
 * - **`postCall`** detects secrets/injection in the result, records them, and
 *   **redacts** leaked secrets out of the content in place so the model never
 *   sees the raw credential.
 */
export class NarcHook implements ToolHook {
    private readonly log: NarcAlert[] = [];

    /** Snapshot every recorded alert. */
    alerts(): NarcAlert[] {
        return [...this.log];
    }

    /** Recorded alerts at or above `minSeverity`. */
    alertsAbove(minSeverity: Severity): NarcAlert[] {
        return this.log.filter((a) => a.severity >= minSeverity);
    }

    private record(alert: NarcAlert): void {
        this.log.push(alert);
    }

    async preCall(call: ToolCall): Promise<void> {
        const argsText = JSON.stringify(call.arguments);

        // Scan all first so every finding is recorded even when one of them blocks.
        let block: NarcFinding | undefined;
        for (const f of scanInjection(argsText)) {
            if (f.severity >= Severity.Block && block === undefined) block = f;
            this.record({ severity: f.severity, category: 'injection', patternName: f.patternName, redacted: f.redacted, toolName: call.name });
        }

        // Secrets in arguments: alert only (may be legitimate).
        for (const f of scanSecrets(argsText)) {
            this.record({ severity: f.severity, category: 'secret', patternName: f.patternName, redacted: f.redacted, toolName: call.name });
        }

        if (block !== undefined) {
            throw new Error(`prompt-injection pattern \`${block.patternName}\` in tool arguments — blocked`);
        }
    }

    async postCall(call: ToolCall, result: ToolResult): Promise<void> {
        // A secret in a tool result is a leak. Record a Block alert AND redact it
        // out of `result.content` — the mutable seam means this rewrite is what
        // the model/conversation and every downstream consumer actually see.
        const secrets = scanSecrets(result.content);
        for (const f of secrets) {
            this.record({ severity: Severity.Block, category: 'secret_leak', patternName: f.patternName, redacted: f.redacted, toolName: call.name });
        }
        if (secrets.length > 0) {
            let content = result.content;
            for (const p of SECRET_PATTERNS) {
                // Replace via a function so `$&`-style sequences in a pattern name
                // could never be interpolated into the output.
                content = content.replace(p.regex, () => `[REDACTED:${p.name}]`);
            }
            result.content = content;
        }
        // Injection in the result is detection + alert only (surveillance) — scan
        // the post-redaction content the model will actually see.
        for (const f of scanInjection(result.content)) {
            this.record({
                severity: f.severity > Severity.Alert ? f.severity : Severity.Alert,
                category: 'injection_output',
                patternName: f.patternName,
                redacted: f.redacted,
                toolName: call.name,
            });
        }
    }
}
