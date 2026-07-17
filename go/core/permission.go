package core

// Native tool-call permission gate for the engine — the Go port of the Rust
// reference `smooth-operator-core::permission` (pearl th-ab0437).
//
// The Go engine registers tools (native + extension-contributed) into the
// agent's dispatch loop as ordinary callables. Before this file there was no
// permission gate: once a tool was registered it ran freely — no allow/ask/deny
// model, no dangerous-command classifier, no circuit-breakers.
//
// [Decide] is the pure, deterministic classifier ported natively from smooth's
// `smooth-bigsmooth::auto_mode` (which cannot be imported here — smooth depends
// on this crate). [PermissionGate] runs it on every gated tool call and blocks
// (returns an error) on a Deny; an Ask is routed to a human approver when one is
// wired and fails closed (blocks) when it is not. This is the security-critical
// core and is exhaustively tested, including adversarial compound-command and
// credential-path inputs.
//
// Persisted allow-list (see permission_grants.go): an Ask that matches a stored
// grant is auto-approved without prompting; answering "approve always" persists
// a new grant. A grant can only upgrade an Ask — it can never waive a Deny
// circuit-breaker.

import (
	"strings"
)

// AutoMode is how aggressively the gate enforces. Mirrors smooth's `AutoMode`
// (a trimmed Claude Code `auto-mode` set). Selected via the SMOOTH_AUTO_MODE env
// var (see AutoModeFromEnv).
type AutoMode int

const (
	// AutoModeAsk is read-only allow, mutating ask, dangerous deny. Default.
	AutoModeAsk AutoMode = iota
	// AutoModeAcceptEdits is like AutoModeAsk but filesystem-edit tools (the
	// Write category) auto-approve instead of asking. Everything else still
	// follows the full engine, and the hard circuit-breakers still block.
	// Mirrors Claude Code's acceptEdits.
	AutoModeAcceptEdits
	// AutoModeDenyUnmatched is like AutoModeAsk but never asks — an unmatched
	// verdict is a deny (fail-closed). The headless / CI posture (Claude Code's
	// dontAsk).
	AutoModeDenyUnmatched
	// AutoModeBypass allows everything except the hard circuit-breakers
	// (rm -rf /, dangerous domains, credential paths, pipe-to-shell, fork bombs,
	// env dumps). Escape hatch equivalent to Claude Code's bypassPermissions,
	// which keeps its circuit-breakers.
	AutoModeBypass
)

// AutoModeFromEnvValue parses a SMOOTH_AUTO_MODE value. Unknown/empty → AutoModeAsk.
func AutoModeFromEnvValue(v string) AutoMode {
	norm := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(v)), "-", ""), "_", "")
	switch norm {
	case "deny", "denyunmatched", "dontask", "headless":
		return AutoModeDenyUnmatched
	case "bypass", "bypasspermissions", "yolo":
		return AutoModeBypass
	case "acceptedits", "acceptedit", "edits":
		return AutoModeAcceptEdits
	default:
		return AutoModeAsk
	}
}

// VerdictKind tags a Verdict.
type VerdictKind int

const (
	// VerdictAllow lets the call through.
	VerdictAllow VerdictKind = iota
	// VerdictDeny blocks the call outright.
	VerdictDeny
	// VerdictAsk pauses to ask a human. With no approver wired, the gate treats
	// this as fail-closed.
	VerdictAsk
)

// Verdict is the pure result returned by [Decide]. Reason carries a
// human/LLM-readable explanation for Deny/Ask (empty for Allow).
type Verdict struct {
	Kind   VerdictKind
	Reason string
}

func allow() Verdict             { return Verdict{Kind: VerdictAllow} }
func deny(reason string) Verdict { return Verdict{Kind: VerdictDeny, Reason: reason} }
func ask(reason string) Verdict  { return Verdict{Kind: VerdictAsk, Reason: reason} }

// ---------------------------------------------------------------------------
// Circuit-breaker data (ported from smooth-narc::judge + auto_mode)
// ---------------------------------------------------------------------------

// dangerousDomainSuffixes are domains we never auto-approve — suffix match,
// case-insensitive.
var dangerousDomainSuffixes = []string{
	".ngrok.io",
	".ngrok-free.app",
	"etherscan.io",
	"blockchain.info",
	"binance.com",
	"pastebin.com",
	"termbin.com",
	"transfer.sh",
}

// dangerousCLISubstrings are shell substrings that must never run — checked
// case-insensitively against the command line and each subcommand.
var dangerousCLISubstrings = []string{
	"rm -rf /",
	"rm -rf ~",
	":(){ :|:& };:",
	"mkfs",
	"dd if=/dev/zero of=/dev/",
	"> /dev/sda",
	"chmod -r 777 /",
	"| sudo sh",
	"systemctl mask",
}

// sensitivePathSubstrings mean "this command touches a credential/sensitive
// path". A match is an immediate deny — reading these to exfil is the
// lethal-trifecta risk, so we block read and write.
var sensitivePathSubstrings = []string{
	".ssh/",
	".aws/credentials",
	".aws/config",
	".config/gh/",
	".config/gcloud",
	".gnupg",
	".kube/config",
	".docker/config.json",
	".npmrc",
	".pypirc",
	".netrc",
	"/etc/shadow",
	"id_rsa",
	"id_ed25519",
	".smooth/providers.json",
	".smooth/auth/",
}

// safeBashBins are read-only command binaries that are always safe. Kept tight —
// anything not here (that isn't explicitly dangerous) falls through to Ask.
var safeBashBins = map[string]struct{}{
	"ls": {}, "cat": {}, "head": {}, "tail": {}, "wc": {}, "grep": {}, "rg": {},
	"fd": {}, "find": {}, "echo": {}, "pwd": {}, "which": {}, "whoami": {},
	"date": {}, "true": {}, "test": {}, "dirname": {}, "basename": {},
	"realpath": {}, "stat": {}, "file": {}, "cksum": {}, "sha256sum": {}, "md5sum": {},
}

// safeGitSubcommands are git subcommands that only read.
var safeGitSubcommands = map[string]struct{}{
	"status": {}, "log": {}, "diff": {}, "show": {}, "branch": {}, "remote": {},
	"rev-parse": {}, "describe": {}, "blame": {}, "ls-files": {},
}

// gitListOnlyFlags are flags under which git branch/remote stay read-only.
var gitListOnlyFlags = map[string]struct{}{
	"-a": {}, "-r": {}, "-v": {}, "-vv": {}, "--all": {}, "--list": {},
	"--verbose": {}, "--show-current": {}, "--merged": {}, "--no-merged": {},
}

// netBashBins are binaries that make outbound network requests.
var netBashBins = map[string]struct{}{
	"curl": {}, "wget": {}, "http": {}, "https": {}, "nc": {}, "ncat": {}, "telnet": {},
}

// shellInterpreters are shell interpreters that execute piped stdin — the sink
// half of a `curl … | sh` exfil-and-run.
var shellInterpreters = map[string]struct{}{
	"sh": {}, "bash": {}, "zsh": {}, "dash": {}, "ksh": {},
}

// sensitiveVarFragments are env-var name fragments whose `$NAME` expansion is
// treated as secret exfiltration when echoed/printed. Substring, case-insensitive.
var sensitiveVarFragments = []string{
	"secret", "token", "password", "passwd", "api_key", "apikey",
	"access_key", "credential", "private_key", "aws_", "ssh_", "session",
}

// transparentWrappers are leading command wrappers that don't change what runs
// (`timeout 5 curl …` → `curl …`).
var transparentWrappers = map[string]struct{}{
	"timeout": {}, "nice": {}, "nohup": {}, "stdbuf": {}, "env": {},
}

// domainMatchesSuffixList matches a domain against a suffix list (exact or
// subdomain), case-insensitive.
func domainMatchesSuffixList(domain string, suffixes []string) bool {
	d := strings.ToLower(domain)
	for _, suffix := range suffixes {
		s := strings.ToLower(suffix)
		if d == s || strings.HasSuffix(d, "."+s) || (strings.HasPrefix(s, ".") && strings.HasSuffix(d, s)) {
			return true
		}
	}
	return false
}

// splitCompound splits a shell command line into subcommands on the operators
// that sequence independent commands: &&, ||, ;, |, &, and newlines. Command /
// process substitution ($(…), `…`, <(…)) is surfaced as its own segment so it
// can't ride in on a safe outer command. Every resulting segment must clear
// policy on its own.
//
// ponytail: substring split, not a real shell lexer — upgrade only if quoting
// edge-cases (echo "a && b") start mattering for policy.
func splitCompound(command string) []string {
	const sep = "\x01"
	normalized := strings.ReplaceAll(command, "&&", sep)
	normalized = strings.ReplaceAll(normalized, "||", sep)
	if strings.Contains(normalized, "$(") || strings.Contains(normalized, "<(") || strings.Contains(normalized, "`") {
		normalized = strings.ReplaceAll(normalized, "$(", sep)
		normalized = strings.ReplaceAll(normalized, "<(", sep)
		normalized = strings.ReplaceAll(normalized, "`", sep)
		normalized = strings.ReplaceAll(normalized, ")", sep)
	}
	fields := strings.FieldsFunc(normalized, func(r rune) bool {
		return r == '\x01' || r == ';' || r == '|' || r == '&' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		s := strings.TrimSpace(f)
		s = strings.Trim(s, `"'`)
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// stripWrappers returns the index of the real command after skipping leading
// transparent wrappers (timeout 5, nice, env, …).
func stripWrappers(tokens []string) int {
	i := 0
	for i < len(tokens) {
		if _, ok := transparentWrappers[tokens[i]]; !ok {
			break
		}
		i++
		for i < len(tokens) && (strings.HasPrefix(tokens[i], "-") || startsWithDigit(tokens[i])) {
			i++
		}
	}
	return i
}

func startsWithDigit(s string) bool {
	return s != "" && s[0] >= '0' && s[0] <= '9'
}

// commandBin returns the first meaningful token of a subcommand (after
// stripping wrappers). Empty string if none.
func commandBin(subcommand string) string {
	tokens := strings.Fields(subcommand)
	start := stripWrappers(tokens)
	if start < len(tokens) {
		return tokens[start]
	}
	return ""
}

// hostFromToken pulls a bare hostname out of a URL-ish or host:port token.
func hostFromToken(tok string) string {
	afterScheme := tok
	if i := strings.Index(tok, "://"); i >= 0 {
		afterScheme = tok[i+3:]
	}
	afterUserinfo := afterScheme
	if i := strings.LastIndex(afterScheme, "@"); i >= 0 {
		afterUserinfo = afterScheme[i+1:]
	}
	// Split on the first path/port/query/fragment delimiter (keep leading empties
	// so a token that starts with a delimiter yields an empty host, matching Rust's
	// split(...).next()).
	host := afterUserinfo
	if i := strings.IndexAny(afterUserinfo, "/:?#"); i >= 0 {
		host = afterUserinfo[:i]
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if host == "localhost" || (strings.Contains(host, ".") && !strings.HasPrefix(host, ".") && !strings.HasSuffix(host, ".")) {
		return strings.ToLower(host)
	}
	return ""
}

// extractHosts extracts candidate hostnames from a single (already split)
// net-tool subcommand. Empty if the binary isn't a net tool.
func extractHosts(subcommand string) []string {
	tokens := strings.Fields(subcommand)
	start := stripWrappers(tokens)
	if start >= len(tokens) {
		return nil
	}
	if _, ok := netBashBins[tokens[start]]; !ok {
		return nil
	}
	var hosts []string
	for _, t := range tokens[start+1:] {
		if strings.HasPrefix(t, "-") {
			continue
		}
		if h := hostFromToken(t); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// sinkBin is the effective binary of a pipe segment, skipping a leading sudo and
// the usual transparent wrappers — so `sudo bash` / `sudo -E sh` are recognised
// as shell sinks.
func sinkBin(segment string) string {
	tokens := strings.Fields(segment)
	i := stripWrappers(tokens)
	for i < len(tokens) && tokens[i] == "sudo" {
		i++
		for i < len(tokens) && strings.HasPrefix(tokens[i], "-") {
			i++
		}
	}
	if i < len(tokens) {
		return tokens[i]
	}
	return ""
}

// isPipeToShell reports whether the whole command line pipes a network fetch
// into a shell interpreter (curl … | sh, wget … | bash). A hard circuit-breaker
// regardless of host — matched structurally across the pipe segments.
func isPipeToShell(command string) bool {
	if !strings.Contains(command, "|") {
		return false
	}
	sawFetch := false
	for _, seg := range strings.Split(command, "|") {
		bin := sinkBin(strings.TrimSpace(seg))
		if bin == "" {
			continue
		}
		if _, isShell := shellInterpreters[bin]; sawFetch && isShell {
			return true
		}
		if _, isNet := netBashBins[bin]; isNet {
			sawFetch = true
		}
	}
	return false
}

// stripWrappersAndSudo strips leading transparent wrappers (timeout 5, nice,
// env, …) and any leading sudo from a single subcommand, returning the remaining
// command text. Used by the deny policy so a rule anchored on the real binary
// (aws …) still matches `sudo aws …` / `timeout 5 aws …`.
func stripWrappersAndSudo(subcommand string) string {
	tokens := strings.Fields(subcommand)
	i := stripWrappers(tokens)
	for i < len(tokens) && tokens[i] == "sudo" {
		i++
		for i < len(tokens) && strings.HasPrefix(tokens[i], "-") {
			i++
		}
	}
	return strings.Join(tokens[i:], " ")
}

// referencesSensitivePath reports whether the command references a sensitive
// credential path.
func referencesSensitivePath(command string) bool {
	lower := strings.ToLower(command)
	for _, p := range sensitivePathSubstrings {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	// .env / .envrc / .env.local dotenv files are secret stores too. Token-scoped
	// so `rg "process.env" src/` isn't flagged.
	for _, t := range strings.Fields(lower) {
		t = strings.Trim(t, `"'();`)
		if strings.HasPrefix(t, ".env") || strings.Contains(t, "/.env") {
			return true
		}
	}
	return false
}

// containsSensitiveVarExpansion reports whether text contains a `$NAME` /
// `${NAME}` expansion whose name matches a sensitiveVarFragments fragment.
func containsSensitiveVarExpansion(text string) bool {
	lower := strings.ToLower(text)
	idx := 0
	for {
		rel := strings.IndexByte(lower[idx:], '$')
		if rel < 0 {
			return false
		}
		start := idx + rel + 1
		j := start
		if j < len(lower) && lower[j] == '{' {
			j++
		}
		nameStart := j
		for j < len(lower) && (isAlnum(lower[j]) || lower[j] == '_') {
			j++
		}
		name := lower[nameStart:j]
		if name != "" {
			for _, f := range sensitiveVarFragments {
				if strings.Contains(name, f) {
					return true
				}
			}
		}
		idx = start
	}
}

func isAlnum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// dumpsEnvironment reports whether this single (already split) subcommand reveals
// the process environment. Matches on intent, not a single binary name (env is
// one spelling of the exfil). Deliberately does NOT match the legitimate setter
// forms (env FOO=bar cmd, export FOO=bar, set -euo pipefail).
func dumpsEnvironment(subcommand string) bool {
	toks := strings.Fields(subcommand)
	if len(toks) == 0 {
		return false
	}
	lower := strings.ToLower(subcommand)
	if strings.Contains(lower, "proc/") && strings.Contains(lower, "/environ") {
		return true
	}
	// Skip transparent wrappers (but NOT env, the subject here).
	i := 0
	for i < len(toks) {
		switch toks[i] {
		case "timeout", "nice", "nohup", "stdbuf":
			i++
			for i < len(toks) && (strings.HasPrefix(toks[i], "-") || startsWithDigit(toks[i])) {
				i++
			}
			continue
		}
		break
	}
	if i >= len(toks) {
		return false
	}
	bin := toks[i]
	rest := toks[i+1:]
	switch bin {
	case "printenv":
		return true
	case "env":
		k := 0
		for k < len(rest) {
			t := rest[k]
			switch {
			case t == "-u" || t == "-S":
				k += 2
			case strings.HasPrefix(t, "-") || strings.Contains(t, "=") || t == "-":
				k++
			default:
				return false // a bare command token → setter form
			}
		}
		return true
	case "export", "declare", "typeset":
		for _, t := range rest {
			if strings.Contains(t, "=") {
				return false
			}
			if !strings.HasPrefix(t, "-") {
				return false
			}
		}
		return true
	case "set":
		return len(rest) == 0
	case "echo", "printf":
		return containsSensitiveVarExpansion(subcommand)
	default:
		return false
	}
}

var findActionFlags = map[string]struct{}{
	"-exec": {}, "-execdir": {}, "-ok": {}, "-okdir": {}, "-delete": {},
	"-fprint": {}, "-fprintf": {}, "-fls": {},
}

// isSafeReadonlyBash reports whether this single subcommand is a compiled-in
// safe read-only command.
func isSafeReadonlyBash(subcommand string) bool {
	bin := commandBin(subcommand)
	if bin == "" {
		return false
	}
	if bin == "find" {
		for _, t := range strings.Fields(subcommand) {
			if _, ok := findActionFlags[t]; ok {
				return false
			}
		}
		return true
	}
	if _, ok := safeBashBins[bin]; ok {
		return true
	}
	if bin == "git" {
		tokens := strings.Fields(subcommand)
		start := stripWrappers(tokens)
		j := start + 1
		for j < len(tokens) && strings.HasPrefix(tokens[j], "-") {
			j += 2 // `-c key=val` / `-C dir`: skip flag + value.
		}
		if j >= len(tokens) {
			return false
		}
		sub := tokens[j]
		if _, ok := safeGitSubcommands[sub]; !ok {
			return false
		}
		if sub == "branch" || sub == "remote" {
			for _, t := range tokens[j+1:] {
				if _, ok := gitListOnlyFlags[t]; !ok {
					return false
				}
			}
		}
		return true
	}
	return false
}

// decideBashSubcommand evaluates a single bash subcommand against the layered policy.
func decideBashSubcommand(subcommand string) Verdict {
	// 1. Credential-path guard — deny read AND write (exfil risk).
	if referencesSensitivePath(subcommand) {
		return deny("command references a sensitive credential path: " + subcommand)
	}
	// 1b. Environment-dump guard — the process env is a secret store.
	if dumpsEnvironment(subcommand) {
		return deny("command reveals the process environment (secret exfiltration risk): " + subcommand)
	}
	// 2. Baseline dangerous-CLI deny (rm -rf /, fork bomb, mkfs, …).
	lower := strings.ToLower(subcommand)
	for _, n := range dangerousCLISubstrings {
		if strings.Contains(lower, strings.ToLower(n)) {
			return deny("command matches dangerous-cli pattern: " + n)
		}
	}
	// 3. Dangerous network hosts referenced by this subcommand → deny.
	hosts := extractHosts(subcommand)
	for _, host := range hosts {
		if domainMatchesSuffixList(host, dangerousDomainSuffixes) {
			return deny(host + " is on the dangerous-domain deny list")
		}
	}
	// 4. Net tool with a non-dangerous host → ask.
	if len(hosts) > 0 {
		return ask("outbound request to " + hosts[0] + " needs approval")
	}
	// 5. Compiled-in safe read-only command → allow.
	if isSafeReadonlyBash(subcommand) {
		return allow()
	}
	// 6. Unmatched mutating command → ask.
	return ask("`" + commandBin(subcommand) + "` is not a known-safe command")
}

// decideBash evaluates a whole (possibly compound) bash command line. Every
// subcommand must clear on its own; the strictest verdict wins (deny > ask > allow).
func decideBash(command string) Verdict {
	// Whole-line dangerous-substring scan FIRST — some breakers (the fork bomb
	// :(){ :|:& };:, | sudo sh) contain the very operators splitCompound divides
	// on, so they must be matched before splitting or they slip through.
	lowerLine := strings.ToLower(command)
	for _, n := range dangerousCLISubstrings {
		if strings.Contains(lowerLine, strings.ToLower(n)) {
			return deny("command matches dangerous-cli pattern: " + n)
		}
	}
	if isPipeToShell(command) {
		return deny("pipe-to-shell execution is blocked: " + command)
	}
	subs := splitCompound(command)
	if len(subs) == 0 {
		return deny("empty command")
	}
	pendingAsk := ""
	havePending := false
	for _, sub := range subs {
		v := decideBashSubcommand(sub)
		switch v.Kind {
		case VerdictDeny:
			return v
		case VerdictAsk:
			if !havePending {
				pendingAsk = v.Reason
				havePending = true
			}
		case VerdictAllow:
		}
	}
	if havePending {
		return ask(pendingAsk)
	}
	return allow()
}

// category is the class a tool falls into, derived from its name.
type category int

const (
	categoryBash category = iota
	categoryNetwork
	categoryWrite
	categorySafe
	categoryUnknown
)

func toolCategory(name string) category {
	// Extension tools are dotted `<ext>.<tool>`; classify on the bare tool name.
	bare := name
	if i := strings.LastIndex(name, "."); i >= 0 {
		bare = name[i+1:]
	}
	n := strings.ToLower(bare)
	switch {
	case n == "bash" || n == "shell" || n == "shell_exec" || n == "run_command":
		return categoryBash
	case strings.Contains(n, "write") || strings.Contains(n, "edit") || strings.Contains(n, "delete") ||
		strings.Contains(n, "remove") || n == "apply_patch" || n == "create_file":
		return categoryWrite
	case strings.Contains(n, "fetch") || strings.Contains(n, "download") || strings.HasPrefix(n, "http"):
		return categoryNetwork
	case strings.HasPrefix(n, "read") || strings.HasPrefix(n, "list") || strings.HasPrefix(n, "get") ||
		strings.Contains(n, "search") || n == "grep" || n == "glob":
		return categorySafe
	default:
		return categoryUnknown
	}
}

// argStr reads the first present string field of args among the given keys.
func argStr(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func decideInner(toolName string, args map[string]any) Verdict {
	switch toolCategory(toolName) {
	case categoryBash:
		cmd := strings.TrimSpace(argStr(args, "cmd", "command"))
		if cmd == "" {
			return deny("bash call with no command")
		}
		return decideBash(cmd)
	case categorySafe:
		// Read-only is not exfil-proof: the read path IS the exfil path.
		for _, key := range []string{"path", "file", "dir", "directory"} {
			if v := argStr(args, key); v != "" && referencesSensitivePath(v) {
				return deny(toolName + " targets a sensitive credential path: " + v)
			}
		}
		return allow()
	case categoryNetwork:
		raw := argStr(args, "url", "host")
		host := hostFromToken(raw)
		if host == "" {
			host = raw
		}
		if host == "" {
			return deny(toolName + " call with no url/host")
		}
		if domainMatchesSuffixList(host, dangerousDomainSuffixes) {
			return deny(host + " is on the dangerous-domain deny list")
		}
		return ask("outbound request to " + host + " needs approval")
	case categoryWrite:
		path := argStr(args, "path", "file")
		if referencesSensitivePath(path) {
			return deny("write to a sensitive credential path: " + path)
		}
		return ask("`" + toolName + "` mutates the filesystem")
	default:
		return ask("`" + toolName + "` is not a recognised safe tool")
	}
}

// Decide is the pure, deterministic permission decision. No I/O — the
// security-critical core, tested exhaustively. args is the parsed tool-call
// argument object; the relevant field is pulled per category (cmd for bash,
// path for writes, url/host for network).
func Decide(mode AutoMode, toolName string, args map[string]any) Verdict {
	// Bypass still honours the hard circuit-breakers: evaluate, then downgrade any
	// Ask to Allow — Deny always survives.
	raw := decideInner(toolName, args)
	if raw.Kind == VerdictDeny {
		return raw
	}
	switch mode {
	case AutoModeBypass:
		return allow()
	case AutoModeAcceptEdits:
		if raw.Kind == VerdictAsk && toolCategory(toolName) == categoryWrite {
			return allow()
		}
		return raw
	case AutoModeDenyUnmatched:
		if raw.Kind == VerdictAsk {
			return deny("headless (no interactive approver): " + raw.Reason)
		}
		return raw
	default:
		return raw
	}
}

// ---------------------------------------------------------------------------
// Grant derivation — map an Ask to a persistable grant and check whether a
// stored grant already covers it. Never derives from a Deny: circuit-breakers
// are not grantable, so grantQuery returns (_, false) for them.
// ---------------------------------------------------------------------------

// grantQuery is the grant that "approve always" would persist for this call — the
// resource the first unresolved Ask is about. ok=false when the call is not an
// Ask (already allowed, or a non-grantable Deny).
func grantQuery(toolName string, args map[string]any) (GrantQuery, bool) {
	switch toolCategory(toolName) {
	case categoryBash:
		cmd := strings.TrimSpace(argStr(args, "cmd", "command"))
		for _, sub := range splitCompound(cmd) {
			v := decideBashSubcommand(sub)
			switch v.Kind {
			case VerdictAsk:
				return bashSegmentGrant(sub), true
			case VerdictDeny:
				return GrantQuery{}, false // a deny sinks the line; nothing grantable
			case VerdictAllow:
			}
		}
		return GrantQuery{}, false
	case categoryNetwork:
		raw := argStr(args, "url", "host")
		host := hostFromToken(raw)
		if host == "" {
			host = raw
		}
		if host == "" {
			return GrantQuery{}, false
		}
		return GrantQuery{Kind: GrantNetwork, Value: host}, true
	case categoryWrite, categoryUnknown:
		return GrantQuery{Kind: GrantTool, Value: toolName}, true
	default: // categorySafe
		return GrantQuery{}, false
	}
}

// bashSegmentGrant is the grant a single asking bash subcommand maps to: a
// network host if it's a net tool, else a `<bin> ` command prefix.
func bashSegmentGrant(sub string) GrantQuery {
	if hosts := extractHosts(sub); len(hosts) > 0 {
		return GrantQuery{Kind: GrantNetwork, Value: hosts[0]}
	}
	return GrantQuery{Kind: GrantBash, Value: commandBin(sub) + " "}
}

// coveredByGrants reports whether this whole tool call is already covered by
// stored grants — so the Ask can be auto-approved without prompting. For
// compound bash, EVERY asking segment must be granted.
func coveredByGrants(grants *PermissionGrants, toolName string, args map[string]any) bool {
	switch toolCategory(toolName) {
	case categoryBash:
		cmd := strings.TrimSpace(argStr(args, "cmd", "command"))
		subs := splitCompound(cmd)
		if len(subs) == 0 {
			return false
		}
		for _, sub := range subs {
			v := decideBashSubcommand(sub)
			switch v.Kind {
			case VerdictAllow:
				continue
			case VerdictDeny:
				return false // never auto-allow a deny
			case VerdictAsk:
				if !bashSegmentGranted(sub, grants) {
					return false
				}
			}
		}
		return true
	case categoryNetwork:
		raw := argStr(args, "url", "host")
		host := hostFromToken(raw)
		if host == "" {
			host = raw
		}
		return host != "" && grants.MatchesHost(host)
	case categoryWrite, categoryUnknown:
		return grants.MatchesTool(toolName)
	default: // categorySafe
		return false
	}
}

// bashSegmentGranted reports whether a single asking bash subcommand is covered
// by a stored grant.
func bashSegmentGranted(sub string, grants *PermissionGrants) bool {
	if hosts := extractHosts(sub); len(hosts) > 0 {
		return grants.MatchesHost(hosts[0])
	}
	return grants.MatchesBash(sub)
}
