package extension

import (
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestEffectiveSubscriptions(t *testing.T) {
	// No declared filter → handshake as-is.
	got := EffectiveSubscriptions(nil, []string{"turn_start", "turn_end"})
	if len(got) != 2 {
		t.Errorf("no-filter passthrough = %v", got)
	}
	// Declared list clamps: tool_call requested but not declared is dropped.
	got = EffectiveSubscriptions([]string{"turn_start"}, []string{"turn_start", "tool_call"})
	if len(got) != 1 {
		t.Errorf("clamp = %v", got)
	}
	if _, ok := got["turn_start"]; !ok {
		t.Errorf("turn_start should survive: %v", got)
	}
	// Declared but not requested → not subscribed.
	got = EffectiveSubscriptions([]string{"turn_start", "turn_end"}, []string{"turn_end"})
	if len(got) != 1 {
		t.Errorf("declared-not-requested = %v", got)
	}
}

func TestHookTypePolicyAndTimeout(t *testing.T) {
	if !HookToolCall.FailClosed() || !HookUserBash.FailClosed() {
		t.Error("tool_call/user_bash must be fail-closed")
	}
	if HookToolResult.FailClosed() || HookMessageEnd.FailClosed() {
		t.Error("tool_result/message_end must be fail-open")
	}
	if HookToolCall.DefaultTimeout() != 60*time.Second || HookToolResult.DefaultTimeout() != 5*time.Second {
		t.Error("default timeouts wrong")
	}
	if h, ok := HookTypeFromName("before_agent_start"); !ok || h != HookBeforeAgentStart {
		t.Errorf("from_name = %v %v", h, ok)
	}
	if _, ok := HookTypeFromName("nope"); ok {
		t.Error("unknown name should not resolve")
	}
}

// ---- FoldHookChain: the security-critical policy, exhaustively ----

func jsonEqual(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

func rawEq(t *testing.T, a, b json.RawMessage) {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		t.Fatalf("bad json %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		t.Fatalf("bad json %s: %v", b, err)
	}
	if !reflect.DeepEqual(va, vb) {
		t.Errorf("json mismatch: got %s want %s", a, b)
	}
}

func TestFoldEmptyChainProceedsUnchanged(t *testing.T) {
	input := json.RawMessage(`{"tool":"rm"}`)
	f := FoldHookChain(HookToolCall, input, nil)
	if f.Blocked {
		t.Fatal("empty chain should not block")
	}
	rawEq(t, f.Value, input)
}

func TestFoldContinueKeepsValue(t *testing.T) {
	steps := []hookStep{repliedStep(HookOutcome{Action: ActionContinue}), repliedStep(HookOutcome{Action: ActionContinue})}
	f := FoldHookChain(HookToolResult, json.RawMessage(`{"a":1}`), steps)
	if f.Blocked {
		t.Fatal("continue should not block")
	}
	rawEq(t, f.Value, json.RawMessage(`{"a":1}`))
}

func TestFoldModifyThreadsPatchToNext(t *testing.T) {
	steps := []hookStep{
		repliedStep(HookOutcome{Action: ActionModify, Patch: json.RawMessage(`{"a":2}`)}),
		repliedStep(HookOutcome{Action: ActionContinue}),
	}
	f := FoldHookChain(HookContext, json.RawMessage(`{"a":1}`), steps)
	rawEq(t, f.Value, json.RawMessage(`{"a":2}`))
}

func TestFoldBlockShortCircuits(t *testing.T) {
	steps := []hookStep{
		repliedStep(HookOutcome{Action: ActionBlock, Reason: "rm -rf blocked"}),
		repliedStep(HookOutcome{Action: ActionModify, Patch: json.RawMessage(`{"should":"not apply"}`)}),
	}
	f := FoldHookChain(HookToolCall, json.RawMessage(`{}`), steps)
	if !f.Blocked || f.Reason != "rm -rf blocked" {
		t.Errorf("expected block, got %+v", f)
	}
}

func TestFoldBlockWithoutReasonGetsDefault(t *testing.T) {
	steps := []hookStep{repliedStep(HookOutcome{Action: ActionBlock})}
	f := FoldHookChain(HookUserBash, json.RawMessage(`{}`), steps)
	if !f.Blocked || f.Reason != "blocked by user_bash hook" {
		t.Errorf("got %+v", f)
	}
}

func TestFoldFailureIsFailClosedForToolCall(t *testing.T) {
	f := FoldHookChain(HookToolCall, json.RawMessage(`{}`), []hookStep{failedStep()})
	if !f.Blocked || !contains(f.Reason, "fail-closed") {
		t.Errorf("expected fail-closed block, got %+v", f)
	}
}

func TestFoldFailureIsFailOpenForOthers(t *testing.T) {
	steps := []hookStep{failedStep(), repliedStep(HookOutcome{Action: ActionContinue})}
	f := FoldHookChain(HookToolResult, json.RawMessage(`{"x":9}`), steps)
	if f.Blocked {
		t.Fatal("fail-open hook should not block on failure")
	}
	rawEq(t, f.Value, json.RawMessage(`{"x":9}`))
}

func TestFoldModifyThenFailureFailOpenKeepsPatch(t *testing.T) {
	steps := []hookStep{repliedStep(HookOutcome{Action: ActionModify, Patch: json.RawMessage(`{"x":2}`)}), failedStep()}
	f := FoldHookChain(HookInput, json.RawMessage(`{"x":1}`), steps)
	rawEq(t, f.Value, json.RawMessage(`{"x":2}`))
}

// ---- HostDelegate defaults ----

func TestDefaultDelegateUIIsNoUI(t *testing.T) {
	_, err := DefaultHostDelegate{}.UIRequest("ext", json.RawMessage(`{"kind":"confirm"}`))
	if err == nil || err.Code != CodeNoUI {
		t.Errorf("expected NoUI, got %v", err)
	}
}

func TestDefaultDelegateExecDenied(t *testing.T) {
	_, err := DefaultHostDelegate{}.ExecRun("ext", json.RawMessage(`{"command":"ls"}`))
	if err == nil || err.Code != CodeNotTrusted {
		t.Errorf("expected NotTrusted, got %v", err)
	}
}

func TestDefaultDelegateAndHostInboundKV(t *testing.T) {
	t.Setenv("SMOOTH_HOME", t.TempDir())

	d := DefaultHostDelegate{}
	if v, _ := d.KVGet("kvtest", "missing"); string(v) != "null" {
		t.Errorf("missing key = %s", v)
	}
	if err := d.KVSet("kvtest", "k", json.RawMessage(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	// The on-disk state file is pretty-printed, so compare semantically.
	if v, _ := d.KVGet("kvtest", "k"); !jsonEqual(v, json.RawMessage(`{"n":1}`)) {
		t.Errorf("kv get = %s", v)
	}

	// Routed through hostInbound (ext→host bridge).
	epoch := &atomic.Uint64{}
	epoch.Store(1)
	inbound := &hostInbound{ext: "e", delegate: DefaultHostDelegate{}, epoch: epoch}
	if _, err := inbound.HandleRequest(MethodPing, nil); err != nil {
		t.Errorf("ping: %v", err)
	}
	if _, err := inbound.HandleRequest("kv/set", json.RawMessage(`{"key":"a","value":5}`)); err != nil {
		t.Fatal(err)
	}
	got, err := inbound.HandleRequest("kv/get", json.RawMessage(`{"key":"a"}`))
	if err != nil || string(got) != `{"value":5}` {
		t.Errorf("kv get = %s %v", got, err)
	}
	if _, err := inbound.HandleRequest("nope/method", nil); err == nil || err.Code != CodeMethodNotFound {
		t.Errorf("expected MethodNotFound, got %v", err)
	}
}

// ---- empty host: the zero-behavior-change default ----

func TestEmptyHostHookIsPassthrough(t *testing.T) {
	h := NewEmptyHost()
	if !h.IsEmpty() {
		t.Fatal("expected empty host")
	}
	f := h.RunHook(t.Context(), HookToolCall, json.RawMessage(`{"tool":"x"}`))
	if f.Blocked {
		t.Fatal("empty host must not block")
	}
	rawEq(t, f.Value, json.RawMessage(`{"tool":"x"}`))
	if got := h.BeforeAgentStart(t.Context(), "prompt"); got != "prompt" {
		t.Errorf("before_agent_start = %q", got)
	}
	if len(h.Tools()) != 0 {
		t.Error("empty host has no tools")
	}
	h.DispatchEvent("turn_start", json.RawMessage(`{}`)) // must not panic
	if len(h.Commands()) != 0 || len(h.Shortcuts()) != 0 {
		t.Error("empty host has no commands/shortcuts")
	}
}

// ---- the command-tier deadlock guard (security-critical), exhaustively ----

func TestTokenEpochParsesOrNone(t *testing.T) {
	cases := []struct {
		token string
		want  uint64
		ok    bool
	}{
		{"epoch-7", 7, true},
		{"epoch-0", 0, true},
		{"epoch-", 0, false},
		{"7", 0, false},
		{"nonce-3", 0, false},
	}
	for _, c := range cases {
		got, ok := tokenEpoch(c.token)
		if got != c.want || ok != c.ok {
			t.Errorf("tokenEpoch(%q) = %d,%v want %d,%v", c.token, got, ok, c.want, c.ok)
		}
	}
}

func ctxParams(tier, token string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"context": map[string]string{"tier": tier, "token": token}, "text": "hi"})
	return b
}

func TestValidateCommandContext(t *testing.T) {
	if err := validateCommandContext(ctxParams("command", "epoch-4"), 4); err != nil {
		t.Errorf("current command tier should pass: %v", err)
	}
	if err := validateCommandContext(ctxParams("event", "epoch-4"), 4); err == nil || err.Code != CodeContextViolation {
		t.Errorf("event tier should be rejected: %v", err)
	}
	if err := validateCommandContext(ctxParams("command", "epoch-4"), 5); err == nil || err.Code != CodeContextViolation {
		t.Errorf("stale epoch should be rejected: %v", err)
	}
	if err := validateCommandContext(json.RawMessage(`{"text":"hi"}`), 1); err == nil || err.Code != CodeContextViolation {
		t.Errorf("missing context should be rejected: %v", err)
	}
	if err := validateCommandContext(ctxParams("command", "garbage"), 1); err == nil || err.Code != CodeContextViolation {
		t.Errorf("malformed token should be rejected: %v", err)
	}
}

// recordingDelegate records which session action fired.
type recordingDelegate struct {
	DefaultHostDelegate
	hits []string
}

func (r *recordingDelegate) SessionSendMessage(string, json.RawMessage) (json.RawMessage, *RpcError) {
	r.hits = append(r.hits, "send_message")
	return json.RawMessage("{}"), nil
}

func (r *recordingDelegate) SessionAppendEntry(string, json.RawMessage) (json.RawMessage, *RpcError) {
	r.hits = append(r.hits, "append_entry")
	return json.RawMessage("{}"), nil
}

func TestHostInboundSessionActionValidatesBeforeDelegate(t *testing.T) {
	delegate := &recordingDelegate{}
	epoch := &atomic.Uint64{}
	epoch.Store(3)
	inbound := &hostInbound{ext: "e", delegate: delegate, epoch: epoch}

	if _, err := inbound.HandleRequest(MethodSessionSendMessage, ctxParams("command", "epoch-3")); err != nil {
		t.Fatalf("valid command context should pass: %v", err)
	}
	if !reflect.DeepEqual(delegate.hits, []string{"send_message"}) {
		t.Errorf("hits = %v", delegate.hits)
	}
	// Event-tier → ContextViolation before the delegate.
	if _, err := inbound.HandleRequest(MethodSessionAppendEntry, ctxParams("event", "epoch-3")); err == nil || err.Code != CodeContextViolation {
		t.Errorf("event tier should violate: %v", err)
	}
	// Stale epoch (reload bumped 3→4) → ContextViolation, delegate untouched.
	epoch.Store(4)
	if _, err := inbound.HandleRequest(MethodSessionSendMessage, ctxParams("command", "epoch-3")); err == nil || err.Code != CodeContextViolation {
		t.Errorf("stale epoch should violate: %v", err)
	}
	if !reflect.DeepEqual(delegate.hits, []string{"send_message"}) {
		t.Errorf("only one valid call should reach the delegate: %v", delegate.hits)
	}
}
