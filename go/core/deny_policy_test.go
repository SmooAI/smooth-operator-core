package core

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func denied(t *testing.T, p *DenyPolicy, name string, args map[string]any) string {
	t.Helper()
	r, ok := p.Evaluate(name, args)
	if !ok {
		t.Fatalf("%s expected to be denied", name)
	}
	return r
}

func notDenied(t *testing.T, p *DenyPolicy, name string, args map[string]any) {
	t.Helper()
	if r, ok := p.Evaluate(name, args); ok {
		t.Fatalf("%s expected to fall through, got deny: %s", name, r)
	}
}

func mustPolicy(t *testing.T, toml string) *DenyPolicy {
	t.Helper()
	p, err := DenyPolicyFromTOML(toml)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ── glob matcher ───────────────────────────────────────────────

func TestGlobExactAndWildcards(t *testing.T) {
	cases := []struct {
		pat, text string
		want      bool
	}{
		{"exact", "exact", true},
		{"exact", "exacts", false},
		{"vendor.*", "vendor.delete", true},
		{"vendor.*", "other.delete", false},
		{"*.delete", "vendor.delete", true},
		{"*.delete", "vendor.deleted", false},
		{"a*c", "abc", true},
		{"a*c", "ac", true},
		{"a*c", "ab", false},
		{"/prod/**", "/prod/secrets/db.txt", true},
		{"/prod/**", "/staging/x", false},
		{"**/secrets/**", "/a/b/secrets/c/d", true},
		{"**/secrets/**", "/a/b/c", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pat, c.text); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pat, c.text, got, c.want)
		}
	}
}

// ── declarative: tools ─────────────────────────────────────────

func TestToolsSectionDeniesMatchAllowsNonmatch(t *testing.T) {
	p := mustPolicy(t, `[tools]
deny = ["vendor.dangerous_tool", "*.delete_prod"]`)
	denied(t, p, "vendor.dangerous_tool", map[string]any{})
	denied(t, p, "svc.delete_prod", map[string]any{})
	notDenied(t, p, "vendor.safe_tool", map[string]any{})
}

// ── declarative: bash ──────────────────────────────────────────

func TestBashSectionDeniesMatchAllowsNonmatch(t *testing.T) {
	p := mustPolicy(t, `[bash]
deny_patterns = ["aws * --profile prod", "terraform apply"]`)
	denied(t, p, "bash", bashArgs("aws s3 ls --profile prod"))
	denied(t, p, "bash", bashArgs("terraform apply -auto-approve"))
	notDenied(t, p, "bash", bashArgs("aws s3 ls --profile dev"))
	notDenied(t, p, "bash", bashArgs("aws s3 ls"))
}

func TestBashPrefixWordBoundary(t *testing.T) {
	p := mustPolicy(t, `[bash]
deny_patterns = ["aws "]`)
	denied(t, p, "bash", bashArgs("aws s3 ls"))
	notDenied(t, p, "bash", bashArgs("awslocal s3 ls"))
}

func TestBashDenySurvivesSudoCompoundAndExtraFlags(t *testing.T) {
	p := mustPolicy(t, `[bash]
deny_patterns = ["aws * --profile prod"]`)
	denied(t, p, "bash", bashArgs("sudo aws s3 rm s3://b --profile prod"))
	denied(t, p, "bash", bashArgs("ls && aws s3 ls --profile prod"))
	denied(t, p, "bash", bashArgs("aws s3 ls --profile prod --region us-east-1"))
	denied(t, p, "bash", bashArgs("timeout 5 aws s3 ls --profile prod"))
}

// ── declarative: network ───────────────────────────────────────

func TestNetworkSectionDeniesSuffixAndGlob(t *testing.T) {
	p := mustPolicy(t, `[network]
deny_hosts = ["*.prod.internal", "prod-*.rds.amazonaws.com", "secrets.example.com"]`)
	denied(t, p, "web_fetch", map[string]any{"url": "https://api.prod.internal/x"})
	denied(t, p, "web_fetch", map[string]any{"url": "https://prod.internal/"})
	denied(t, p, "web_fetch", map[string]any{"url": "https://prod-db1.rds.amazonaws.com"})
	denied(t, p, "web_fetch", map[string]any{"host": "api.secrets.example.com"})
	notDenied(t, p, "web_fetch", map[string]any{"url": "https://staging.internal/x"})
	denied(t, p, "bash", bashArgs("curl https://api.prod.internal/health"))
}

// ── declarative: paths ─────────────────────────────────────────

func TestPathsSectionDeniesWriteAndRead(t *testing.T) {
	p := mustPolicy(t, `[paths]
deny = ["/prod/**", "**/secrets/**"]`)
	denied(t, p, "file_write", map[string]any{"path": "/prod/config.yaml"})
	denied(t, p, "read_file", map[string]any{"path": "/app/secrets/db.env"})
	denied(t, p, "list_dir", map[string]any{"dir": "/prod/data"})
	notDenied(t, p, "file_write", map[string]any{"path": "/app/src/main.rs"})
}

// ── predicate tier ─────────────────────────────────────────────

type prodAccountPredicate struct{}

func (prodAccountPredicate) Evaluate(_ string, args map[string]any) (DenyReason, bool) {
	cmd, _ := args["cmd"].(string)
	if strings.Contains(cmd, "999999999999") {
		return NewDenyReason("resolved to the prod AWS account"), true
	}
	return DenyReason{}, false
}

func TestPredicateSomeDeniesNoneFallsThrough(t *testing.T) {
	p := NewDenyPolicy().WithPredicate(prodAccountPredicate{})
	r := denied(t, p, "bash", bashArgs("aws s3 ls --profile acct-999999999999"))
	if !strings.Contains(r, "prod AWS account") {
		t.Errorf("predicate reason should surface, got %s", r)
	}
	notDenied(t, p, "bash", bashArgs("aws s3 ls --profile acct-111"))
}

// ── empty policy = no-op ───────────────────────────────────────

func TestEmptyPolicyDeniesNothing(t *testing.T) {
	p := NewDenyPolicy()
	if !p.IsEmpty() {
		t.Error("new policy must be empty")
	}
	notDenied(t, p, "bash", bashArgs("rm -rf /prod"))
	notDenied(t, p, "file_write", map[string]any{"path": "/prod/x"})
	notDenied(t, p, "vendor.anything", map[string]any{})
}

// ── TOML round-trip ────────────────────────────────────────────

func TestDenyRulesTOMLRoundTrip(t *testing.T) {
	rules := NewDenyRules()
	rules.Tools.Deny = []string{"vendor.dangerous_tool"}
	rules.Bash.DenyPatterns = []string{"aws * --profile prod"}
	rules.Network.DenyHosts = []string{"*.prod.internal"}
	rules.Paths.Deny = []string{"/prod/**"}
	text, err := rules.ToTOMLString()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseDenyRules(text)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tools.Deny[0] != "vendor.dangerous_tool" || got.Bash.DenyPatterns[0] != "aws * --profile prod" ||
		got.Network.DenyHosts[0] != "*.prod.internal" || got.Paths.Deny[0] != "/prod/**" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestEmptyRulesParseAndAreEmpty(t *testing.T) {
	r, err := ParseDenyRules("")
	if err != nil || !r.IsEmpty() {
		t.Errorf("empty parse: %v empty=%v", err, r.IsEmpty())
	}
	r, err = ParseDenyRules("schema_version = 1")
	if err != nil || !r.IsEmpty() {
		t.Errorf("schema-only parse: %v empty=%v", err, r.IsEmpty())
	}
}

// ── precedence: declarative before predicate ───────────────────

type alwaysDeny struct{}

func (alwaysDeny) Evaluate(_ string, _ map[string]any) (DenyReason, bool) {
	return NewDenyReason("predicate always denies"), true
}

func TestDeclarativeReasonWinsOverPredicate(t *testing.T) {
	p := mustPolicy(t, `[tools]
deny = ["vendor.tool"]`).WithPredicate(alwaysDeny{})
	if r := denied(t, p, "vendor.tool", map[string]any{}); !strings.Contains(r, "(tools)") {
		t.Errorf("declarative must win, got %s", r)
	}
	if r := denied(t, p, "other.tool", map[string]any{}); !strings.Contains(r, "(predicate)") {
		t.Errorf("predicate must fire on non-declarative-match, got %s", r)
	}
}

// ── gate wiring: precedence over grants + modes ────────────────

func TestEmptyDenyPolicyIsAdditiveNoop(t *testing.T) {
	g := &PermissionGate{Mode: AutoModeAsk, DenyPolicy: NewDenyPolicy()}
	if err := g.Check(context.Background(), "bash", bashArgs("ls -la")); err != nil {
		t.Errorf("allow-today must stay allow: %v", err)
	}
	if err := g.Check(context.Background(), "bash", bashArgs("npm install x")); err == nil {
		t.Error("ask-today must still fail closed")
	}
	if err := g.Check(context.Background(), "bash", bashArgs("rm -rf /")); err == nil {
		t.Error("circuit-breaker must still deny")
	}
}

func TestDenyPolicyBlocksMatchingCallUnderBypass(t *testing.T) {
	p := mustPolicy(t, "[bash]\ndeny_patterns = [\"terraform apply\"]")
	g := &PermissionGate{Mode: AutoModeBypass, DenyPolicy: p}
	err := g.Check(context.Background(), "bash", bashArgs("terraform apply -auto-approve"))
	if err == nil || !strings.Contains(err.Error(), "permission denied") || !strings.Contains(err.Error(), "denied by policy") {
		t.Errorf("policy deny must win under bypass, got %v", err)
	}
	if err := g.Check(context.Background(), "bash", bashArgs("terraform plan")); err != nil {
		t.Errorf("non-matching command must pass under bypass: %v", err)
	}
}

func TestDenyPolicyBeatsAStoredGrant(t *testing.T) {
	grants := NewPermissionGrants()
	grants.Add(GrantQuery{Kind: GrantBash, Value: "terraform "})
	p := mustPolicy(t, "[bash]\ndeny_patterns = [\"terraform apply\"]")
	g := &PermissionGate{
		Mode:        AutoModeAsk,
		Grants:      NewSharedGrants(grants),
		PersistPath: filepath.Join(t.TempDir(), "w.toml"),
		DenyPolicy:  p,
	}
	if err := g.Check(context.Background(), "bash", bashArgs("terraform apply")); err == nil {
		t.Error("a grant must not waive a deny-policy match")
	}
}

type writerEndpointPredicate struct{}

func (writerEndpointPredicate) Evaluate(_ string, args map[string]any) (DenyReason, bool) {
	cmd, _ := args["cmd"].(string)
	if strings.Contains(cmd, "writer.db") {
		return NewDenyReason("db writer endpoint is off-limits; use the read replica"), true
	}
	return DenyReason{}, false
}

func TestDenyPolicyPredicateBeatsGrantAndSurvivesBypass(t *testing.T) {
	grants := NewPermissionGrants()
	grants.Add(GrantQuery{Kind: GrantBash, Value: "psql "})
	p := NewDenyPolicy().WithPredicate(writerEndpointPredicate{})
	g := &PermissionGate{
		Mode:        AutoModeBypass,
		Grants:      NewSharedGrants(grants),
		PersistPath: filepath.Join(t.TempDir(), "w.toml"),
		DenyPolicy:  p,
	}
	err := g.Check(context.Background(), "bash", bashArgs("psql -h writer.db.internal -c 'select 1'"))
	if err == nil || !strings.Contains(err.Error(), "read replica") {
		t.Errorf("predicate reason should surface, got %v", err)
	}
	if err := g.Check(context.Background(), "bash", bashArgs("psql -h replica.db.internal -c 'select 1'")); err != nil {
		t.Errorf("read-replica connection must pass under bypass: %v", err)
	}
}
