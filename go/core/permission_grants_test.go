package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrantsNewPinsSchemaVersionOne(t *testing.T) {
	if (&PermissionGrants{}).SchemaVersion != 0 {
		t.Error("zero value schema must be 0")
	}
	if NewPermissionGrants().SchemaVersion != 1 {
		t.Error("New must pin schema 1")
	}
}

func TestGrantsHostExactAndWildcard(t *testing.T) {
	g := NewPermissionGrants()
	g.Add(GrantQuery{Kind: GrantNetwork, Value: "api.example.com"})
	if !g.MatchesHost("api.example.com") || !g.MatchesHost("API.EXAMPLE.COM") {
		t.Error("exact host must match case-insensitively")
	}
	if g.MatchesHost("other.example.com") {
		t.Error("exact host must not match a different subdomain")
	}
	w := NewPermissionGrants()
	w.Add(GrantQuery{Kind: GrantNetwork, Value: "*.example.com"})
	if !w.MatchesHost("api.example.com") || !w.MatchesHost("example.com") {
		t.Error("wildcard must match subdomain and apex")
	}
	if w.MatchesHost("evil-example.com") {
		t.Error("wildcard must not match evil-example.com")
	}
}

func TestGrantsBareHostRequiresExactMatch(t *testing.T) {
	g := NewPermissionGrants()
	g.Add(GrantQuery{Kind: GrantNetwork, Value: "example.com"})
	if !g.MatchesHost("example.com") {
		t.Error("bare host must match itself")
	}
	if g.MatchesHost("api.example.com") || g.MatchesHost("evil-example.com") {
		t.Error("bare host must not match subdomain/substring")
	}
}

func TestGrantsToolExactOnly(t *testing.T) {
	g := NewPermissionGrants()
	g.Add(GrantQuery{Kind: GrantTool, Value: "web_search"})
	if !g.MatchesTool("web_search") {
		t.Error("tool must match exactly")
	}
	if g.MatchesTool("web_search_v2") {
		t.Error("tool must not prefix-match")
	}
}

func TestGrantsBashPrefixTrailingSpaceGuard(t *testing.T) {
	g := NewPermissionGrants()
	g.Add(GrantQuery{Kind: GrantBash, Value: "cargo "})
	if !g.MatchesBash("cargo test") || !g.MatchesBash("CARGO BUILD") {
		t.Error("bash prefix must match case-insensitively")
	}
	if g.MatchesBash("cargonaut") {
		t.Error("trailing space must guard against cargonaut")
	}
}

func TestGrantsContainsMatchesAdd(t *testing.T) {
	g := NewPermissionGrants()
	q := GrantQuery{Kind: GrantBash, Value: "npm "}
	if g.Contains(q) {
		t.Error("empty grants must not contain q")
	}
	g.Add(q)
	if !g.Contains(q) {
		t.Error("after add, contains must be true")
	}
}

func TestGrantsMergeUnions(t *testing.T) {
	a := NewPermissionGrants()
	a.Add(GrantQuery{Kind: GrantNetwork, Value: "a.example.com"})
	b := NewPermissionGrants()
	b.Add(GrantQuery{Kind: GrantTool, Value: "t"})
	b.Add(GrantQuery{Kind: GrantBash, Value: "pnpm "})
	a.MergeWith(b)
	if !a.MatchesHost("a.example.com") || !a.MatchesTool("t") || !a.MatchesBash("pnpm i") {
		t.Error("merge must union all sections")
	}
}

func TestGrantsSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wonk-allow.toml")
	g := NewPermissionGrants()
	g.Add(GrantQuery{Kind: GrantNetwork, Value: "*.openai.com"})
	g.Add(GrantQuery{Kind: GrantTool, Value: "web_search"})
	g.Add(GrantQuery{Kind: GrantBash, Value: "cargo "})
	if err := g.SaveToPath(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadPermissionGrants(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.MatchesHost("api.openai.com") || !loaded.MatchesTool("web_search") || !loaded.MatchesBash("cargo test") {
		t.Errorf("round-trip lost data: %+v", loaded)
	}
}

func TestGrantsLoadMissingIsEmptyNotError(t *testing.T) {
	g, err := LoadPermissionGrants(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if g.SchemaVersion != 1 || len(g.Network.AllowHosts) != 0 {
		t.Error("missing file must yield empty v1 store")
	}
}

func TestGrantsLoadMalformedSurfacesError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wonk-allow.toml")
	if err := os.WriteFile(path, []byte("this is [not valid = toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPermissionGrants(path)
	if err == nil || !strings.Contains(err.Error(), "malformed wonk-allow.toml") {
		t.Errorf("want malformed error, got %v", err)
	}
}

func TestGrantsSaveAtomicAndCreatesDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "d", "wonk-allow.toml")
	g := NewPermissionGrants()
	g.Add(GrantQuery{Kind: GrantNetwork, Value: "a.example.com"})
	if err := g.SaveToPath(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error("file must exist")
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("tempfile must be renamed away")
	}
}

func TestAppendGrantCreatesThenExtendsIdempotently(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wonk-allow.toml")
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(AppendGrant(path, GrantQuery{Kind: GrantBash, Value: "npm "}))
	must(AppendGrant(path, GrantQuery{Kind: GrantBash, Value: "npm "})) // dup
	must(AppendGrant(path, GrantQuery{Kind: GrantNetwork, Value: "api.example.com"}))
	g, err := LoadPermissionGrants(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Bash.AllowPatterns) != 1 {
		t.Errorf("dup must not duplicate, got %v", g.Bash.AllowPatterns)
	}
	if !g.MatchesBash("npm install left-pad") || !g.MatchesHost("api.example.com") {
		t.Error("appended grants must match")
	}
}

func TestLoadLayeredProjectWinsSchemaButUnionsEntries(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user.toml")
	project := filepath.Join(dir, "project.toml")
	u := NewPermissionGrants()
	u.Add(GrantQuery{Kind: GrantBash, Value: "cargo "})
	if err := u.SaveToPath(user); err != nil {
		t.Fatal(err)
	}
	p := NewPermissionGrants()
	p.Add(GrantQuery{Kind: GrantBash, Value: "pnpm "})
	p.Add(GrantQuery{Kind: GrantTool, Value: "web_search"})
	if err := p.SaveToPath(project); err != nil {
		t.Fatal(err)
	}
	merged, err := LoadLayeredGrants(user, project)
	if err != nil {
		t.Fatal(err)
	}
	if !merged.MatchesBash("cargo test") || !merged.MatchesBash("pnpm i") || !merged.MatchesTool("web_search") {
		t.Error("layered load must union entries")
	}
}

func TestSharedSnapshotIsolatedAndMergeVisible(t *testing.T) {
	shared := NewSharedGrants(NewPermissionGrants())
	more := NewPermissionGrants()
	more.Add(GrantQuery{Kind: GrantNetwork, Value: "b.example.com"})
	shared.MergeIn(more)
	if !shared.Snapshot().MatchesHost("b.example.com") {
		t.Error("merged host must be visible")
	}
	snap := shared.Snapshot()
	snap.Add(GrantQuery{Kind: GrantNetwork, Value: "c.example.com"})
	if shared.Snapshot().MatchesHost("c.example.com") {
		t.Error("mutating a snapshot must not touch the store")
	}
}

func TestGrantsPathHelpers(t *testing.T) {
	if p := UserGrantsPath(); p != "" && !strings.HasSuffix(p, filepath.Join(".smooth", "wonk-allow.toml")) {
		t.Errorf("unexpected user path: %s", p)
	}
	if got := ProjectGrantsPath("/tmp/x"); got != filepath.Join("/tmp/x", ".smooth", "wonk-allow.toml") {
		t.Errorf("unexpected project path: %s", got)
	}
}

// ── grant derivation + coverage (drives the gate's Ask flow) ────

func TestGrantQueryMapsAskShapes(t *testing.T) {
	check := func(name string, args map[string]any, wantKind GrantKind, wantVal string, wantOK bool) {
		q, ok := grantQuery(name, args)
		if ok != wantOK {
			t.Fatalf("%s: ok=%v want %v", name, ok, wantOK)
		}
		if ok && (q.Kind != wantKind || q.Value != wantVal) {
			t.Errorf("%s: got %v/%q want %v/%q", name, q.Kind, q.Value, wantKind, wantVal)
		}
	}
	check("bash", bashArgs("npm install x"), GrantBash, "npm ", true)
	check("bash", bashArgs("curl https://new.example.com/x"), GrantNetwork, "new.example.com", true)
	check("web_fetch", map[string]any{"url": "https://new.example.com/x"}, GrantNetwork, "new.example.com", true)
	check("file_write", map[string]any{"path": "/tmp/x"}, GrantTool, "file_write", true)
	check("mystery_tool", map[string]any{}, GrantTool, "mystery_tool", true)
	check("bash", bashArgs("ls"), 0, "", false)       // not an ask
	check("bash", bashArgs("rm -rf /"), 0, "", false) // deny is never grantable
	check("read_file", map[string]any{}, 0, "", false)
}

func TestStoredGrantAutoAllowsWithoutPrompting(t *testing.T) {
	grants := NewPermissionGrants()
	grants.Add(GrantQuery{Kind: GrantBash, Value: "npm "})
	called := false
	g := &PermissionGate{
		Mode:   AutoModeAsk,
		Grants: NewSharedGrants(grants),
		Approver: func(_ context.Context, _ HumanApprovalRequest) (HumanApprovalResponse, error) {
			called = true
			return Deny("should not be asked"), nil
		},
		PersistPath: filepath.Join(t.TempDir(), "wonk-allow.toml"),
	}
	if err := g.Check(context.Background(), "bash", bashArgs("npm install left-pad")); err != nil {
		t.Errorf("granted command must auto-allow: %v", err)
	}
	if called {
		t.Error("a stored grant must not prompt")
	}
}

func TestApproveAlwaysPersistsThenSecondAskIsSilent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wonk-allow.toml")
	shared := NewSharedGrants(NewPermissionGrants())

	hook1 := &PermissionGate{
		Mode:        AutoModeAsk,
		Grants:      shared,
		PersistPath: path,
		Approver:    approver(ApproveAlways(), nil),
	}
	if err := hook1.Check(context.Background(), "bash", bashArgs("npm install x")); err != nil {
		t.Fatalf("first ask (approve-always) must pass: %v", err)
	}

	onDisk, err := LoadPermissionGrants(path)
	if err != nil {
		t.Fatal(err)
	}
	if !onDisk.MatchesBash("npm install x") {
		t.Error("grant should be persisted to disk")
	}
	if !shared.Snapshot().MatchesBash("npm run build") {
		t.Error("grant should be merged into the live view")
	}

	// Second call: NO approver — must still pass via the persisted grant.
	hook2 := &PermissionGate{Mode: AutoModeAsk, Grants: shared, PersistPath: path}
	if err := hook2.Check(context.Background(), "bash", bashArgs("npm run build")); err != nil {
		t.Errorf("second identical-prefix ask must auto-allow: %v", err)
	}
}

func TestStoredGrantCannotWaiveADeny(t *testing.T) {
	grants := NewPermissionGrants()
	grants.Add(GrantQuery{Kind: GrantBash, Value: "rm "})
	grants.Add(GrantQuery{Kind: GrantNetwork, Value: "pastebin.com"})
	g := &PermissionGate{Mode: AutoModeAsk, Grants: NewSharedGrants(grants), PersistPath: filepath.Join(t.TempDir(), "w.toml")}
	if err := g.Check(context.Background(), "bash", bashArgs("rm -rf /")); err == nil {
		t.Error("rm -rf / must stay denied despite the rm grant")
	}
	if err := g.Check(context.Background(), "bash", bashArgs("curl https://pastebin.com/raw/x")); err == nil {
		t.Error("dangerous-domain deny must not be waived by a host grant")
	}
}

func TestApproveAlwaysWithoutGrantsIsJustApproveOnce(t *testing.T) {
	// No Grants/PersistPath — approve-always degrades to approve-once.
	g := &PermissionGate{Mode: AutoModeAsk, Approver: approver(ApproveAlways(), nil)}
	if err := g.Check(context.Background(), "bash", bashArgs("npm install x")); err != nil {
		t.Errorf("approve-always without grants must still pass: %v", err)
	}
}

func TestPartialCompoundGrantStillPrompts(t *testing.T) {
	grants := NewPermissionGrants()
	grants.Add(GrantQuery{Kind: GrantBash, Value: "npm "}) // only npm granted
	// No approver → an Ask fails closed. If coverage wrongly returned true this
	// would pass; it must not.
	g := &PermissionGate{Mode: AutoModeAsk, Grants: NewSharedGrants(grants), PersistPath: filepath.Join(t.TempDir(), "w.toml")}
	if err := g.Check(context.Background(), "bash", bashArgs("npm install x && yarn build")); err == nil {
		t.Error("ungranted second segment must still require approval")
	}
}

func TestAppendGrantPersistsForReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wonk-allow.toml")
	if err := AppendGrant(path, GrantQuery{Kind: GrantTool, Value: "web_search"}); err != nil {
		t.Fatal(err)
	}
	g, err := LoadPermissionGrants(path)
	if err != nil {
		t.Fatal(err)
	}
	if !g.MatchesTool("web_search") {
		t.Error("appended tool grant must reload")
	}
}
