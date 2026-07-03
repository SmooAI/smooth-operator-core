package extension

// SEP conformance replay — the Go host's side of the shared fixture suite.
//
// The canonical fixtures live in the smooth-operator repo at
// spec/extension/conformance/fixtures.json and are validated there against the
// JSON Schemas by the TypeScript conformance test. This package vendors a copy
// (testdata/fixtures.json) and asserts the Go protocol types agree with it:
// every typed method fixture unmarshals with the right fields, and the $invalid
// instances that violate a Go-enforced constraint are rejected. Provider-phase
// and schema-only invalids are covered by the spec repo's TS conformance, not by
// the engine's permissive typed round-trip (mirrors the Rust sep_conformance).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type fixture struct {
	SchemaRef string          `json:"$schema_ref"`
	Instance  json.RawMessage `json:"instance"`
}

type invalidFixture struct {
	Name      string          `json:"name"`
	SchemaRef string          `json:"$schema_ref"`
	Instance  json.RawMessage `json:"instance"`
}

func loadFixtures(t *testing.T) (map[string]fixture, []invalidFixture) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "fixtures.json"))
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	valid := map[string]fixture{}
	for name, body := range all {
		if name == "$invalid" || name[0] == '$' {
			continue
		}
		var f fixture
		if err := json.Unmarshal(body, &f); err != nil {
			t.Fatalf("parse fixture %s: %v", name, err)
		}
		valid[name] = f
	}
	var invalid []invalidFixture
	if raw, ok := all["$invalid"]; ok {
		if err := json.Unmarshal(raw, &invalid); err != nil {
			t.Fatalf("parse $invalid: %v", err)
		}
	}
	return valid, invalid
}

func instance(t *testing.T, valid map[string]fixture, name string) json.RawMessage {
	t.Helper()
	f, ok := valid[name]
	if !ok {
		t.Fatalf("fixture %q missing", name)
	}
	return f.Instance
}

func TestFixtureSetIsPresent(t *testing.T) {
	valid, _ := loadFixtures(t)
	if len(valid) < 40 {
		t.Fatalf("expected the full SEP fixture set, found %d", len(valid))
	}
}

func TestLifecycleFixturesRoundTripIntoTypes(t *testing.T) {
	valid, _ := loadFixtures(t)

	t.Run("initialize_params", func(t *testing.T) {
		var p InitializeParams
		mustUnmarshal(t, instance(t, valid, "initialize_params"), &p)
		if p.ProtocolVersion != 1 {
			t.Errorf("protocol_version = %d", p.ProtocolVersion)
		}
		if p.Host.Name != "smooth-operator-core" {
			t.Errorf("host.name = %q", p.Host.Name)
		}
		if !p.Workspace.Trusted {
			t.Error("workspace.trusted should be true")
		}
		if len(p.UICapabilities) == 0 || p.UICapabilities[0] != "select" {
			t.Errorf("ui_capabilities = %v", p.UICapabilities)
		}
		if err := p.Validate(); err != nil {
			t.Errorf("valid params should pass validation: %v", err)
		}
	})

	t.Run("initialize_result", func(t *testing.T) {
		var r InitializeResult
		mustUnmarshal(t, instance(t, valid, "initialize_result"), &r)
		if r.ProtocolVersion != 1 || r.Extension.Name == "" {
			t.Errorf("result = %+v", r)
		}
		if len(r.Registrations.Tools) == 0 || r.Registrations.Tools[0].Name == "" || len(r.Registrations.Tools[0].Parameters) == 0 {
			t.Errorf("tools = %+v", r.Registrations.Tools)
		}
		if len(r.Registrations.Subscriptions) == 0 {
			t.Errorf("subscriptions = %v", r.Registrations.Subscriptions)
		}
	})

	t.Run("tool_execute_params", func(t *testing.T) {
		var p ToolExecuteParams
		mustUnmarshal(t, instance(t, valid, "tool_execute_params"), &p)
		if p.CallID != "call-1" || p.Tool != "say" {
			t.Errorf("params = %+v", p)
		}
		var args struct {
			Phrase string `json:"phrase"`
		}
		mustUnmarshal(t, p.Arguments, &args)
		if args.Phrase != "hello" {
			t.Errorf("arguments.phrase = %q", args.Phrase)
		}
		if p.Context.Tier != TierCommand {
			t.Errorf("context.tier = %q", p.Context.Tier)
		}
		if err := p.Validate(); err != nil {
			t.Errorf("valid params should pass validation: %v", err)
		}
	})

	t.Run("tool_execute_result", func(t *testing.T) {
		var r ToolExecuteResult
		mustUnmarshal(t, instance(t, valid, "tool_execute_result"), &r)
		if r.Content != "hello" || r.IsError {
			t.Errorf("result = %+v", r)
		}
	})

	t.Run("tool_update_params", func(t *testing.T) {
		var p ToolUpdateParams
		mustUnmarshal(t, instance(t, valid, "tool_update_params"), &p)
		if p.CallID != "call-1" || p.Message != "working..." {
			t.Errorf("params = %+v", p)
		}
		if p.Progress == nil || *p.Progress != 0.5 {
			t.Errorf("progress = %v", p.Progress)
		}
	})

	t.Run("hook_outcomes", func(t *testing.T) {
		var c HookOutcome
		mustUnmarshal(t, instance(t, valid, "hook_outcome_continue"), &c)
		if c.Action != ActionContinue {
			t.Errorf("continue = %+v", c)
		}
		var b HookOutcome
		mustUnmarshal(t, instance(t, valid, "hook_outcome_block"), &b)
		if b.Action != ActionBlock || b.Reason != "rm -rf blocked" {
			t.Errorf("block = %+v", b)
		}
		var m HookOutcome
		mustUnmarshal(t, instance(t, valid, "hook_outcome_modify"), &m)
		if m.Action != ActionModify || len(m.Patch) == 0 {
			t.Errorf("modify = %+v", m)
		}
	})
}

func TestEventFixturesSeqSemantics(t *testing.T) {
	valid, _ := loadFixtures(t)

	var normal EventParams
	mustUnmarshal(t, instance(t, valid, "event_params"), &normal)
	if normal.Seq == nil {
		t.Error("a dispatched event is seq-numbered")
	}
	if normal.Event != "turn_start" {
		t.Errorf("event = %q", normal.Event)
	}

	var lost EventParams
	mustUnmarshal(t, instance(t, valid, "event_events_lost"), &lost)
	if lost.Event != "events_lost" {
		t.Errorf("event = %q", lost.Event)
	}
	if lost.Seq != nil {
		t.Error("the events_lost marker is out-of-band (no seq)")
	}
	var payload struct {
		Lost uint64 `json:"lost"`
	}
	mustUnmarshal(t, lost.Payload, &payload)
	if payload.Lost != 12 {
		t.Errorf("payload.lost = %d", payload.Lost)
	}
}

func TestFrameFixturesParseAndClassify(t *testing.T) {
	valid, _ := loadFixtures(t)

	var req Message
	mustUnmarshal(t, instance(t, valid, "frame_request"), &req)
	if !req.IsRequest() {
		t.Error("frame_request should classify as a request")
	}
	var note Message
	mustUnmarshal(t, instance(t, valid, "frame_notification"), &note)
	if !note.IsNotification() {
		t.Error("frame_notification should classify as a notification")
	}
	var ok Message
	mustUnmarshal(t, instance(t, valid, "frame_success_response"), &ok)
	if !ok.IsResponse() || len(ok.Result) == 0 {
		t.Error("frame_success_response should classify as a response with a result")
	}
	for _, name := range []string{"frame_error_response", "error_blocked", "error_cancelled", "error_context_violation"} {
		var m Message
		mustUnmarshal(t, instance(t, valid, name), &m)
		if m.Error == nil {
			t.Errorf("%s should carry an error object", name)
		}
	}
}

func TestInvalidFixturesRejected(t *testing.T) {
	_, invalid := loadFixtures(t)
	find := func(name string) json.RawMessage {
		for _, e := range invalid {
			if e.Name == name {
				return e.Instance
			}
		}
		t.Fatalf("invalid fixture %q missing", name)
		return nil
	}

	// Required field missing → typed validation rejects.
	var ip InitializeParams
	_ = json.Unmarshal(find("initialize_params_missing_protocol_version"), &ip)
	if err := ip.Validate(); err == nil {
		t.Error("missing protocol_version should fail validation")
	}
	var tep ToolExecuteParams
	_ = json.Unmarshal(find("tool_execute_params_missing_call_id"), &tep)
	if err := tep.Validate(); err == nil {
		t.Error("missing call_id should fail validation")
	}
	// Tagged-union constraints on HookOutcome.
	var h HookOutcome
	if err := json.Unmarshal(find("hook_outcome_bogus_action"), &h); err == nil {
		t.Error("bogus hook action should be rejected")
	}
	if err := json.Unmarshal(find("hook_outcome_modify_missing_patch"), &h); err == nil {
		t.Error("modify-missing-patch should be rejected")
	}
	// Frame-level invalids (jsonrpc "1.0", missing method) are rejected by the
	// JSON Schema, not the permissive envelope type — covered by the spec repo's
	// TS conformance, so not asserted here.
}

func mustUnmarshal(t *testing.T, raw json.RawMessage, target any) {
	t.Helper()
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("unmarshal into %T: %v", target, err)
	}
}
