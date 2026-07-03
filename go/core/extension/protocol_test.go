package extension

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMessageClassification(t *testing.T) {
	req := NewRequest(json.RawMessage("1"), "ping", json.RawMessage("{}"))
	if !req.IsRequest() || req.IsNotification() || req.IsResponse() {
		t.Errorf("request misclassified: %+v", req)
	}
	note := NewNotification("event", json.RawMessage("{}"))
	if !note.IsNotification() || note.IsRequest() {
		t.Errorf("notification misclassified: %+v", note)
	}
	ok := NewSuccess(json.RawMessage("1"), json.RawMessage("{}"))
	if !ok.IsResponse() || ok.IsRequest() {
		t.Errorf("success misclassified: %+v", ok)
	}
	errResp := NewErrorResponse(json.RawMessage("1"), NewRpcError(CodeBlocked, "no"))
	if !errResp.IsResponse() {
		t.Errorf("error response misclassified: %+v", errResp)
	}
}

func TestRequestFrameOmitsResultAndError(t *testing.T) {
	b, err := json.Marshal(NewRequest(json.RawMessage("7"), "tool/execute", json.RawMessage(`{"x":1}`)))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, bad := range []string{`"result"`, `"error"`} {
		if contains(s, bad) {
			t.Errorf("request frame should not contain %s: %s", bad, s)
		}
	}
	if !contains(s, `"jsonrpc":"2.0"`) || !contains(s, `"method":"tool/execute"`) {
		t.Errorf("request frame missing fields: %s", s)
	}
}

func TestNotificationHasNoID(t *testing.T) {
	b, _ := json.Marshal(NewNotification("event", json.RawMessage(`{"event":"turn_start"}`)))
	if contains(string(b), `"id"`) {
		t.Errorf("notification should have no id: %s", b)
	}
}

func TestMessageRoundtripsAllShapes(t *testing.T) {
	for _, m := range []Message{
		NewRequest(json.RawMessage(`"abc"`), "initialize", json.RawMessage("{}")),
		NewNotification("log", json.RawMessage(`{"level":"info","message":"hi"}`)),
		NewSuccess(json.RawMessage("2"), json.RawMessage(`{"ok":true}`)),
		NewErrorResponse(json.RawMessage("2"), NewRpcError(CodeCancelled, "cancelled")),
		NewErrorResponse(nil, NewRpcError(CodeParseError, "bad json")),
	} {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var back Message
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(m, back) {
			t.Errorf("round-trip changed frame:\n got %+v\nwant %+v", back, m)
		}
	}
}

func TestHookOutcomeMarshalsByAction(t *testing.T) {
	cases := []struct {
		o    HookOutcome
		want string
	}{
		{HookOutcome{Action: ActionContinue}, `{"action":"continue"}`},
		{HookOutcome{Action: ActionBlock, Reason: "nope"}, `{"action":"block","reason":"nope"}`},
		{HookOutcome{Action: ActionBlock}, `{"action":"block"}`},
		{HookOutcome{Action: ActionModify, Patch: json.RawMessage(`{"a":1}`)}, `{"action":"modify","patch":{"a":1}}`},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.o)
		if err != nil {
			t.Fatalf("marshal %+v: %v", c.o, err)
		}
		if string(b) != c.want {
			t.Errorf("marshal %+v = %s, want %s", c.o, b, c.want)
		}
	}
}

func TestHookOutcomeParsesFromWire(t *testing.T) {
	var c HookOutcome
	if err := json.Unmarshal([]byte(`{"action":"continue"}`), &c); err != nil || c.Action != ActionContinue {
		t.Errorf("continue parse: %v %+v", err, c)
	}
	var m HookOutcome
	if err := json.Unmarshal([]byte(`{"action":"modify","patch":{}}`), &m); err != nil || m.Action != ActionModify {
		t.Errorf("modify parse: %v %+v", err, m)
	}
}

func TestHookOutcomeRejectsInvalid(t *testing.T) {
	var h HookOutcome
	if err := json.Unmarshal([]byte(`{"action":"bogus"}`), &h); err == nil {
		t.Error("expected unknown action to be rejected")
	}
	if err := json.Unmarshal([]byte(`{"action":"modify"}`), &h); err == nil {
		t.Error("expected modify-missing-patch to be rejected")
	}
}

func TestValidateRequiredFields(t *testing.T) {
	var missing InitializeParams
	_ = json.Unmarshal([]byte(`{"host":{"name":"h","version":"1"},"workspace":{"root":"/","trusted":true},"mode":"tui"}`), &missing)
	if err := missing.Validate(); err == nil {
		t.Error("expected missing protocol_version to fail validation")
	}
	var tep ToolExecuteParams
	_ = json.Unmarshal([]byte(`{"tool":"say","arguments":{},"context":{"token":"t","tier":"command"}}`), &tep)
	if err := tep.Validate(); err == nil {
		t.Error("expected missing call_id to fail validation")
	}
}

func TestRpcErrorIsError(t *testing.T) {
	e := NewRpcError(CodeNoUI, "headless")
	if e.Error() != "JSON-RPC error -32001: headless" {
		t.Errorf("Error() = %q", e.Error())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
