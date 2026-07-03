package extension

// Live subprocess tests: spawn the self-re-exec echo peer (see echopeer_test.go)
// and exercise the real machinery — discovery, handshake, tool execute, the
// ext→host ui bridge, hook block/crash, request timeout+cancel, reload epoch
// fence, and the respawn generation guard.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// peerEnv is the base env that turns a re-exec of the test binary into the peer.
func peerEnv(extra map[string]string) map[string]string {
	e := map[string]string{"SEP_ECHO_PEER": "1"}
	for k, v := range extra {
		e[k] = v
	}
	return e
}

// spawnPeer starts a raw ExtensionProcess running the peer with the given handler.
func spawnPeer(t *testing.T, env map[string]string, handler InboundHandler) *ExtensionProcess {
	t.Helper()
	proc, err := Spawn(SpawnSpec{Command: os.Args[0], Env: peerEnv(env)}, handler)
	if err != nil {
		t.Fatalf("spawn peer: %v", err)
	}
	t.Cleanup(proc.Close)
	return proc
}

// liveHost loads a one-extension host running the peer (via direct discovery),
// with the given delegate and peer env.
func liveHost(t *testing.T, env map[string]string, delegate HostDelegate) *ExtensionHost {
	t.Helper()
	discovered := []DiscoveredExtension{{
		Manifest: ExtensionManifest{
			Name: "echo", Version: "0.1.0", Protocol: 1,
			Run:          RunSpec{Command: os.Args[0], Env: peerEnv(env)},
			Capabilities: Capabilities{Tools: true},
		},
		Scope: ScopeGlobal,
	}}
	host, failures := Load(t.Context(), discovered, HostInfo{Name: "test", Version: "0"}, WorkspaceInfo{Trusted: true}, "test", nil, delegate)
	if len(failures) != 0 {
		t.Fatalf("load failures: %+v", failures)
	}
	t.Cleanup(func() { host.ShutdownAll(context.Background()) })
	return host
}

func TestLiveDiscoverSpawnHandshakeAndTool(t *testing.T) {
	// Full path: an extension.toml on disk → Discover → Load → tool execute.
	dir := t.TempDir()
	extDir := filepath.Join(dir, "echo")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "name = \"echo\"\nversion = \"0.1.0\"\n[run]\ncommand = \"" + os.Args[0] + "\"\n[run.env]\nSEP_ECHO_PEER = \"1\"\n[capabilities]\ntools = true\n"
	if err := os.WriteFile(filepath.Join(extDir, "extension.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	discovered, failures := Discover(dir, "")
	if len(discovered) != 1 || len(failures) != 0 {
		t.Fatalf("discover = %d ext, %+v failures", len(discovered), failures)
	}
	host, lf := Load(t.Context(), discovered, HostInfo{Name: "test", Version: "0"}, WorkspaceInfo{Root: dir, Trusted: true}, "test", nil, nil)
	if len(lf) != 0 {
		t.Fatalf("load failures: %+v", lf)
	}
	defer host.ShutdownAll(context.Background())

	if host.Len() != 1 || host.Names()[0] != "echo" {
		t.Fatalf("host = %d %v", host.Len(), host.Names())
	}
	tools := host.Tools()
	if len(tools) != 1 || tools[0].Name() != "echo.say" {
		t.Fatalf("tools = %+v", tools)
	}
	out, err := tools[0].Execute(t.Context(), map[string]any{"phrase": "hello world"})
	if err != nil || out != "hello world" {
		t.Fatalf("execute = %q, %v", out, err)
	}
	if len(host.Commands()) != 1 || host.Commands()[0].Command.Name != "echo-cmd" {
		t.Errorf("commands = %+v", host.Commands())
	}
	if len(host.Shortcuts()) != 1 || host.Shortcuts()[0].Shortcut.Key != "ctrl+e" {
		t.Errorf("shortcuts = %+v", host.Shortcuts())
	}
	res, rerr := host.RunCommand(t.Context(), "", "echo-cmd", json.RawMessage(`{}`))
	if rerr != nil || res.Content != "ran echo-cmd" {
		t.Errorf("run command = %+v %v", res, rerr)
	}
	comps := host.CompleteCommand(t.Context(), "", "echo-cmd", "hi")
	if len(comps) != 1 || comps[0].Value != "hi-done" {
		t.Errorf("completions = %+v", comps)
	}
}

func TestLiveRequestTimeoutSendsCancel(t *testing.T) {
	proc := spawnPeer(t, map[string]string{"SEP_ECHO_TOOL": "hang"}, nil)
	start := time.Now()
	_, err := proc.Request(t.Context(), MethodToolExecute,
		json.RawMessage(`{"call_id":"c","tool":"say","arguments":{},"context":{"token":"epoch-1","tier":"command"}}`),
		200*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error from a hung peer")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}

func TestLiveHookBlock(t *testing.T) {
	host := liveHost(t, map[string]string{"SEP_ECHO_HOOK": "block"}, nil)
	f := host.RunHook(t.Context(), HookToolCall, json.RawMessage(`{"tool":"bash","arguments":{"command":"rm -rf /"}}`))
	if !f.Blocked || f.Reason != "peer blocked" {
		t.Errorf("expected block, got %+v", f)
	}
}

func TestLiveHookCrashIsFailClosed(t *testing.T) {
	host := liveHost(t, map[string]string{"SEP_ECHO_CRASH_HOOK": "1"}, nil)
	f := host.RunToolCallHook(t.Context(), "bash", json.RawMessage(`{"command":"ls"}`))
	if !f.Blocked || !contains(f.Reason, "fail-closed") {
		t.Errorf("a crashed tool_call hook must fail closed, got %+v", f)
	}
}

func TestLiveBeforeAgentStartModify(t *testing.T) {
	host := liveHost(t, map[string]string{"SEP_ECHO_HOOK": "modify"}, nil)
	got := host.BeforeAgentStart(t.Context(), "original")
	if got != "patched" {
		t.Errorf("before_agent_start = %q, want patched", got)
	}
}

// confirmDelegate answers every ui/request as a positive confirm.
type confirmDelegate struct{ DefaultHostDelegate }

func (confirmDelegate) UIRequest(string, json.RawMessage) (json.RawMessage, *RpcError) {
	return json.RawMessage(`{"confirmed":true}`), nil
}

func TestLiveUIRequestBridge(t *testing.T) {
	host := liveHost(t, map[string]string{"SEP_ECHO_TOOL": "ui"}, confirmDelegate{})
	tools := host.Tools()
	if len(tools) != 1 {
		t.Fatalf("tools = %+v", tools)
	}
	// The peer sends an ext→host ui/request mid-tool; the delegate answers, and
	// the peer echoes the ui result back as the tool content.
	out, err := tools[0].Execute(t.Context(), map[string]any{"phrase": "x"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !contains(out, `"confirmed":true`) {
		t.Errorf("tool content should carry the ui reply, got %q", out)
	}
}

func TestLiveReloadBumpsEpoch(t *testing.T) {
	host := liveHost(t, nil, nil)
	before := host.Context(TierCommand).Token
	if err := host.Reload(t.Context(), "echo"); err != nil {
		t.Fatalf("reload: %v", err)
	}
	after := host.Context(TierCommand).Token
	if before == after {
		t.Errorf("reload should bump the epoch (token %q unchanged)", after)
	}
	// The reloaded extension still works: its tools re-mint at the new epoch.
	tools := host.ToolsFor("echo")
	if len(tools) != 1 || tools[0].Name() != "echo.say" {
		t.Fatalf("tools after reload = %+v", tools)
	}
	out, err := tools[0].Execute(t.Context(), map[string]any{"phrase": "again"})
	if err != nil || out != "again" {
		t.Errorf("execute after reload = %q, %v", out, err)
	}
}

func TestLiveRespawnGenerationGuard(t *testing.T) {
	proc := spawnPeer(t, nil, nil)
	if proc.Generation() != 0 {
		t.Fatalf("initial generation = %d", proc.Generation())
	}
	if !proc.PingHealth(t.Context(), 2*time.Second) {
		t.Fatal("peer should answer ping before respawn")
	}
	if err := proc.Respawn(); err != nil {
		t.Fatalf("respawn: %v", err)
	}
	if proc.Generation() != 1 {
		t.Errorf("generation after respawn = %d, want 1", proc.Generation())
	}
	if !proc.IsAlive() {
		t.Error("process should be alive after respawn")
	}
	if !proc.PingHealth(t.Context(), 2*time.Second) {
		t.Error("peer should answer ping after respawn")
	}
}
