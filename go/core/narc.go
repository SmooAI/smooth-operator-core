package core

// Native secret-detection + prompt-injection scanning ToolHook — the Go port of
// the Rust reference engine's `narc.rs` (pearl th-5f7227).
//
// The SEP ExtensionHost passes tool-call arguments to the extension subprocess
// UNSCANNED and returns the subprocess's tool-result content to the model
// VERBATIM. Nothing at the extension boundary looks for leaked credentials or
// prompt-injection payloads. [NarcHook] closes that gap. It scans two things:
//
//   - Secrets — 10 credential patterns (AWS keys, private keys, JWTs/bearer
//     tokens, high-entropy provider keys, …).
//   - Prompt injection — 8 patterns (instruction override, role hijack,
//     jailbreak, data/URL exfiltration, …).
//
// Division of labour with [PermissionGate]: the permission gate owns the
// dangerous-command / write / credential-path circuit-breakers (rm -rf /,
// curl | sh, ~/.ssh/id_rsa). Narc does NOT re-implement those — it is scoped to
// the one thing permission does not do: CONTENT SCANNING of arguments and
// results for secrets and injection. Install Narc AFTER the permission gate so
// the allow/ask/deny decision happens first and Narc scans what clears it.
//
// PreCall (arguments) — blocks on exfiltration, alerts otherwise. A
// [SeverityBlock] injection match (the active data/URL exfiltration signals)
// returns an error, blocking the call before it reaches the tool. Lower-severity
// injection and ANY secret in the arguments are alerted, not blocked — a tool
// argument legitimately carrying a secret (writing a .env, configuring a client)
// is common enough that a hard block there would be a footgun.
//
// PostCall (result) — detects, alerts, and REDACTS secrets. PostCall takes a
// *ToolResult, so a hook's rewrite of Content is what downstream consumers — and
// the LLM/conversation — actually see. A secret pattern in a tool result raises a
// [SeverityBlock] alert AND replaces the matched credential with
// `[REDACTED:<pattern-name>]` in the content before it reaches the model.
// Injection patterns in the result remain detection + [SeverityAlert] only
// (surveillance) — they can appear in legitimate content and are not rewritten.
//
// The detection set is pinned across all five engines by the shared corpus at
// spec/narc/corpus.json (see narc_test.go).

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
)

// Severity is how severe a Narc finding is, ordered least → most severe. A
// [SeverityBlock] finding in PreCall blocks the tool call.
type Severity int

const (
	// SeverityInfo is informational — no action.
	SeverityInfo Severity = iota
	// SeverityWarn is suspicious but plausibly legitimate (e.g. a secret in an argument).
	SeverityWarn
	// SeverityAlert is a strong signal worth surfacing, but not auto-blocked.
	SeverityAlert
	// SeverityBlock is actively harmful — blocks the call when raised in PreCall.
	SeverityBlock
)

// String renders the severity as the shared wire label (INFO/WARN/ALERT/BLOCK).
func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "INFO"
	case SeverityWarn:
		return "WARN"
	case SeverityAlert:
		return "ALERT"
	case SeverityBlock:
		return "BLOCK"
	default:
		return "INFO"
	}
}

// NarcAlert is a single surveillance finding. Lean by design — the log line
// supplies the timestamp, so no uuid/timestamp fields are carried.
type NarcAlert struct {
	// Severity is how severe the finding is.
	Severity Severity
	// Category is the coarse bucket: "injection", "secret", "secret_leak", "injection_output".
	Category string
	// PatternName is the named pattern that matched.
	PatternName string
	// Redacted is a redacted view of the matched text (never the raw secret).
	Redacted string
	// ToolName is the tool whose args/result triggered the finding.
	ToolName string
}

// NarcFinding is a pattern match: which pattern, its severity, and a redacted view.
type NarcFinding struct {
	// PatternName is the named pattern that matched.
	PatternName string
	// Severity is the finding's severity.
	Severity Severity
	// Redacted is a redacted view of the matched text (safe to log).
	Redacted string
}

// narcPattern is one named detector.
type narcPattern struct {
	name     string
	severity Severity
	re       *regexp.Regexp
}

// secretPatterns is the 10 credential patterns. All are [SeverityWarn] in
// arguments (may be legit) and escalate to [SeverityBlock] when found in a
// result (a leak) — the caller decides which threshold to apply.
var secretPatterns = []narcPattern{
	{"AWS Access Key", SeverityWarn, regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"AWS Secret Key", SeverityWarn, regexp.MustCompile(`(?i)aws[_\-]?secret[_\-]?access[_\-]?key\s*[=:]\s*[A-Za-z0-9/+=]{40}`)},
	{"Anthropic API Key", SeverityWarn, regexp.MustCompile(`sk-ant-[A-Za-z0-9\-_]{20,}`)},
	{"OpenAI API Key", SeverityWarn, regexp.MustCompile(`sk-[A-Za-z0-9]{20,}`)},
	{"GitHub Token", SeverityWarn, regexp.MustCompile(`gh[posr]_[A-Za-z0-9_]{36,}`)},
	{"Private Key", SeverityWarn, regexp.MustCompile(`-----BEGIN\s+(RSA\s+)?PRIVATE\s+KEY-----`)},
	{"Generic Secret", SeverityWarn, regexp.MustCompile(`(?i)(secret|password|token|api[_\-]?key)\s*[=:]\s*["']?[A-Za-z0-9/+=\-_]{8,}`)},
	{"Bearer Token", SeverityWarn, regexp.MustCompile(`Bearer\s+[A-Za-z0-9\-_.~+/]+=*`)},
	{"Base64 Encoded Key", SeverityWarn, regexp.MustCompile(`(?i)(key|secret|password)\s*[=:]\s*[A-Za-z0-9+/]{32,}={0,2}`)},
	{"Stripe Key", SeverityWarn, regexp.MustCompile(`[sr]k_(live|test)_[A-Za-z0-9]{20,}`)},
}

// injectionPatterns is the 8 prompt-injection patterns. Only the active
// data/URL exfiltration signals are [SeverityBlock] (blocked in arguments);
// hijack/jailbreak text is [SeverityAlert] (surveilled, not blocked — it can
// appear in legitimate content the model is authoring, e.g. a security test or
// documentation about injection).
//
// data_exfiltration is the Rust pattern's free-spacing `(?x)` form flattened to
// one line: Go's RE2 has no `x` flag, and a rewrite that dropped an alternative
// would silently weaken the detector.
var injectionPatterns = []narcPattern{
	{"ignore_instructions", SeverityAlert, regexp.MustCompile(`(?i)ignore\s+(all\s+)?(previous|prior|above)\s+(instructions|prompts|rules)`)},
	{"role_hijack", SeverityAlert, regexp.MustCompile(`(?i)(you\s+are\s+now|act\s+as|pretend\s+(to\s+be|you\s+are)|from\s+now\s+on\s+you\s+are)`)},
	{"system_prompt", SeverityAlert, regexp.MustCompile(`(?i)(system\s*:\s*|<\|system\|>|\[SYSTEM\])`)},
	{"jailbreak", SeverityAlert, regexp.MustCompile(`(?i)(DAN\s+mode|developer\s+mode|do\s+anything\s+now|jailbreak)`)},
	{"base64_smuggling", SeverityAlert, regexp.MustCompile(`(?i)(decode|eval|execute)\s+(this\s+)?(base64|encoded)`)},
	{"data_exfiltration", SeverityBlock, regexp.MustCompile(`(?i)(send|post|upload|exfiltrate|transmit|leak|push)\s+(all\s+|the\s+|our\s+|my\s+|this\s+)*(data|files?|secrets?|credentials?|keys?|tokens?|contents?|env\s+(vars?|file)|package\.json|\.env|pyproject\.toml|cargo\.toml|requirements\.txt|gemfile|go\.mod|composer\.json|\.ssh/[a-z_]+|id_rsa|\.aws/[a-z]+|\.gnupg/)\s+(to|via|at|over)`)},
	{"url_exfiltration", SeverityBlock, regexp.MustCompile(`(?i)(send|post|upload|push|transmit|leak|exfiltrate)\b[^.\n]{1,200}\s+(to|via|at|over)\s+(https?://[\w.\-/]+)`)},
	{"smell_url", SeverityAlert, regexp.MustCompile(`(?i)https?://[\w.\-]*\b(leak|exfil|attacker|evil|tracker|c2(?:server)?|webhook\.site)\b[\w.\-/]*`)},
}

// scan runs every pattern over text and returns one finding per match, in
// pattern order.
func scan(patterns []narcPattern, text string) []NarcFinding {
	out := []NarcFinding{}
	for _, p := range patterns {
		for _, m := range p.re.FindAllString(text, -1) {
			out = append(out, NarcFinding{PatternName: p.name, Severity: p.severity, Redacted: RedactMatch(m)})
		}
	}
	return out
}

// ScanSecrets scans text for hardcoded secrets. Every match is redacted.
func ScanSecrets(text string) []NarcFinding { return scan(secretPatterns, text) }

// ScanInjection scans text for prompt-injection patterns. Matched text is redacted.
func ScanInjection(text string) []NarcFinding { return scan(injectionPatterns, text) }

// HasSecrets reports whether text contains any secret pattern.
func HasSecrets(text string) bool {
	for _, p := range secretPatterns {
		if p.re.MatchString(text) {
			return true
		}
	}
	return false
}

// HasInjection reports whether text contains any injection pattern.
func HasInjection(text string) bool {
	for _, p := range injectionPatterns {
		if p.re.MatchString(text) {
			return true
		}
	}
	return false
}

// RedactMatch redacts a matched string, showing only the first 4 and last 2
// characters. Short matches (≤ 8 runes) are fully starred. Runes, not bytes —
// parity with the Rust reference's char-based redaction.
func RedactMatch(s string) string {
	r := []rune(s)
	if len(r) <= 8 {
		return strings.Repeat("*", len(r))
	}
	return string(r[:4]) + strings.Repeat("*", len(r)-6) + "**" + string(r[len(r)-2:])
}

// NarcHook is a [ToolHook] that scans tool-call arguments and results for
// secrets and prompt injection. Install it on AgentOptions.Hooks alongside the
// permission gate, AFTER it, so the permission gate decides allow/ask/deny first
// and Narc scans the calls that clear it.
//
//   - PreCall blocks on a [SeverityBlock] injection pattern in the arguments
//     (active exfiltration); every other finding (lower-severity injection, any
//     secret) is recorded as a [NarcAlert] and logged, not blocked.
//   - PostCall detects secrets/injection in the result, records + logs them, and
//     REDACTS leaked secrets out of the content in place so the model never sees
//     the raw credential.
//
// Safe for concurrent use: with ParallelToolCalls the engine may run
// PreCall/PostCall from several goroutines at once.
type NarcHook struct {
	mu     sync.Mutex
	alerts []NarcAlert
}

// NewNarcHook builds a fresh hook with an empty alert log.
func NewNarcHook() *NarcHook { return &NarcHook{} }

// Alerts snapshots every recorded alert.
func (h *NarcHook) Alerts() []NarcAlert {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]NarcAlert(nil), h.alerts...)
}

// AlertsAbove returns the recorded alerts at or above minSeverity.
func (h *NarcHook) AlertsAbove(minSeverity Severity) []NarcAlert {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []NarcAlert{}
	for _, a := range h.alerts {
		if a.Severity >= minSeverity {
			out = append(out, a)
		}
	}
	return out
}

func (h *NarcHook) record(a NarcAlert) {
	log.Printf("narc: %s finding tool=%s category=%s pattern=%s redacted=%s",
		a.Severity, a.ToolName, a.Category, a.PatternName, a.Redacted)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.alerts = append(h.alerts, a)
}

// PreCall scans the tool arguments. A Block-severity injection pattern (active
// exfiltration) returns an error and the tool never runs; everything else is
// recorded and allowed.
func (h *NarcHook) PreCall(_ context.Context, call ToolCall) error {
	args := call.Arguments

	// Scan all first so every finding is recorded even when one of them blocks.
	var block *NarcFinding
	for _, f := range ScanInjection(args) {
		if f.Severity >= SeverityBlock && block == nil {
			hit := f
			block = &hit
		}
		h.record(NarcAlert{Severity: f.Severity, Category: "injection", PatternName: f.PatternName, Redacted: f.Redacted, ToolName: call.Name})
	}

	// Secrets in arguments: alert only (may be legitimate).
	for _, f := range ScanSecrets(args) {
		h.record(NarcAlert{Severity: f.Severity, Category: "secret", PatternName: f.PatternName, Redacted: f.Redacted, ToolName: call.Name})
	}

	if block != nil {
		return fmt.Errorf("prompt-injection pattern %q in tool arguments — blocked", block.PatternName)
	}
	return nil
}

// PostCall scans the tool result. A secret in a result is a leak: it raises a
// Block alert AND is redacted out of result.Content in place. Injection in the
// result is surveillance only — recorded, never rewritten.
func (h *NarcHook) PostCall(_ context.Context, call ToolCall, result *ToolResult) error {
	secrets := ScanSecrets(result.Content)
	for _, f := range secrets {
		h.record(NarcAlert{Severity: SeverityBlock, Category: "secret_leak", PatternName: f.PatternName, Redacted: f.Redacted, ToolName: call.Name})
	}
	if len(secrets) > 0 {
		content := result.Content
		for _, p := range secretPatterns {
			content = p.re.ReplaceAllLiteralString(content, "[REDACTED:"+p.name+"]")
		}
		result.Content = content
	}
	// Scan the POST-redaction content — what the model will actually see.
	for _, f := range ScanInjection(result.Content) {
		sev := f.Severity
		if sev < SeverityAlert {
			sev = SeverityAlert
		}
		h.record(NarcAlert{Severity: sev, Category: "injection_output", PatternName: f.PatternName, Redacted: f.Redacted, ToolName: call.Name})
	}
	return nil
}
