package extension

import (
	"os"
	"path/filepath"
	"testing"
)

const minimalManifest = `
name = "echo"
version = "0.1.0"
[run]
command = "node"
args = ["echo.mjs"]
`

func TestParsesMinimalManifestWithDefaults(t *testing.T) {
	m, err := ParseManifest(minimalManifest)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "echo" || m.Protocol != 1 || m.Run.Command != "node" {
		t.Errorf("unexpected manifest: %+v", m)
	}
	if len(m.Run.Args) != 1 || m.Run.Args[0] != "echo.mjs" {
		t.Errorf("args = %v", m.Run.Args)
	}
	if m.Disabled || len(m.Capabilities.Events) != 0 {
		t.Errorf("expected defaults: %+v", m)
	}
}

func TestParsesFullManifest(t *testing.T) {
	text := `
name = "gate"
version = "2.0.0"
protocol = 1
hook_timeout_ms = 3000
[run]
command = "python3"
args = ["-m", "gate"]
env = { TOKEN = "${env:GATE_TOKEN}", STATIC = "x" }
[capabilities]
events = ["turn_start", "tool_call"]
tools = true
ui = true
[resources]
skills = "skills"
`
	m, err := ParseManifest(text)
	if err != nil {
		t.Fatal(err)
	}
	if m.HookTimeoutMS != 3000 {
		t.Errorf("hook_timeout_ms = %d", m.HookTimeoutMS)
	}
	if !m.Capabilities.Tools || !m.Capabilities.UI || m.Capabilities.Exec {
		t.Errorf("capabilities = %+v", m.Capabilities)
	}
	if len(m.Capabilities.Events) != 2 || m.Capabilities.Events[0] != "turn_start" {
		t.Errorf("events = %v", m.Capabilities.Events)
	}
	if m.Resources.Skills != "skills" {
		t.Errorf("skills = %q", m.Resources.Skills)
	}
}

func TestMalformedManifestErrors(t *testing.T) {
	if _, err := ParseManifest("not toml : : :"); err == nil {
		t.Error("expected malformed toml to error")
	}
	// Missing required fields.
	if _, err := ParseManifest(`name = "x"` + "\n"); err == nil {
		t.Error("expected missing version/command to error")
	}
}

func TestResolvedEnvExpandsRefs(t *testing.T) {
	t.Setenv("SEP_TEST_TOKEN", "secret123")
	text := `
name = "e"
version = "1"
[run]
command = "c"
env = { A = "pre-${env:SEP_TEST_TOKEN}-post", B = "${env:SEP_TEST_UNSET_XYZ}" }
`
	m, err := ParseManifest(text)
	if err != nil {
		t.Fatal(err)
	}
	env := m.ResolvedEnv()
	if env["A"] != "pre-secret123-post" {
		t.Errorf("A = %q", env["A"])
	}
	if env["B"] != "" {
		t.Errorf("B = %q (unset should expand to empty)", env["B"])
	}
}

func TestExpandEnvHandlesUnterminatedRef(t *testing.T) {
	if got := expandEnv("a${env:FOO"); got != "a${env:FOO" {
		t.Errorf("got %q", got)
	}
	if got := expandEnv("plain"); got != "plain" {
		t.Errorf("got %q", got)
	}
}

func writeExt(t *testing.T, dir, name, body string) {
	t.Helper()
	extDir := filepath.Join(dir, name)
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "extension.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverMergesProjectOverGlobal(t *testing.T) {
	tmp := t.TempDir()
	global := filepath.Join(tmp, "global")
	project := filepath.Join(tmp, "project")

	writeExt(t, global, "echo", "name=\"echo\"\nversion=\"1.0.0\"\n[run]\ncommand=\"g\"\n")
	writeExt(t, global, "only_global", "name=\"only_global\"\nversion=\"1\"\n[run]\ncommand=\"g\"\n")
	writeExt(t, project, "echo", "name=\"echo\"\nversion=\"2.0.0\"\n[run]\ncommand=\"p\"\n")
	writeExt(t, project, "only_project", "name=\"only_project\"\nversion=\"1\"\n[run]\ncommand=\"p\"\n")

	found, failures := Discover(global, project)
	if len(failures) != 0 {
		t.Fatalf("failures: %v", failures)
	}
	if len(found) != 3 {
		t.Fatalf("found %d, want 3: %+v", len(found), found)
	}
	var echo *DiscoveredExtension
	for i := range found {
		if found[i].Manifest.Name == "echo" {
			echo = &found[i]
		}
	}
	if echo == nil || echo.Manifest.Version != "2.0.0" || echo.Scope != ScopeProject {
		t.Errorf("project echo should win: %+v", echo)
	}
	// Deterministic order (sorted by name) so hook chaining is stable.
	if found[0].Manifest.Name != "echo" || found[1].Manifest.Name != "only_global" || found[2].Manifest.Name != "only_project" {
		t.Errorf("not sorted: %v", []string{found[0].Manifest.Name, found[1].Manifest.Name, found[2].Manifest.Name})
	}
}

func TestDiscoverToleratesOneBrokenManifest(t *testing.T) {
	tmp := t.TempDir()
	global := filepath.Join(tmp, "g")
	writeExt(t, global, "good", "name=\"good\"\nversion=\"1\"\n[run]\ncommand=\"c\"\n")
	writeExt(t, global, "bad", "this is not = = valid toml\n[[[")

	found, failures := Discover(global, "")
	if len(found) != 1 || found[0].Manifest.Name != "good" {
		t.Errorf("found = %+v", found)
	}
	if len(failures) != 1 || !contains(failures[0].Source, "bad") {
		t.Errorf("failures = %+v", failures)
	}
}

func TestDiscoverMissingDirsIsEmptyNotError(t *testing.T) {
	found, failures := Discover("/no/such/global", "/no/such/project")
	if len(found) != 0 || len(failures) != 0 {
		t.Errorf("found=%v failures=%v", found, failures)
	}
}
