package extension

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestBackoffSchedule(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
		ok      bool
	}{
		{0, 1 * time.Second, true},
		{1, 5 * time.Second, true},
		{2, 25 * time.Second, true},
		{3, 0, false},
	}
	for _, c := range cases {
		got, ok := BackoffFor(c.attempt)
		if got != c.want || ok != c.ok {
			t.Errorf("BackoffFor(%d) = %v,%v want %v,%v", c.attempt, got, ok, c.want, c.ok)
		}
	}
}

func TestObserveLaneShedsOldestAndMarksLoss(t *testing.T) {
	lane := newObserveLane()
	ctx := json.RawMessage(`{"token":"e","tier":"event"}`)
	// Overflow by 3: push CAP+3 events.
	for i := 0; i < ObserveQueueCap+3; i++ {
		lane.push("turn_start", ctx, json.RawMessage(`{"n":`+strconv.Itoa(i)+`}`))
	}
	lane.mu.Lock()
	qlen := len(lane.queue)
	lost := lane.lost
	seq := lane.seq
	lane.mu.Unlock()
	if qlen != ObserveQueueCap {
		t.Errorf("queue len = %d, want %d", qlen, ObserveQueueCap)
	}
	if lost != 3 {
		t.Errorf("lost = %d, want 3", lost)
	}
	if seq != uint64(ObserveQueueCap+3) {
		t.Errorf("seq = %d, want %d", seq, ObserveQueueCap+3)
	}

	// First drain frame is the events_lost marker carrying the shed count and no seq.
	marker, ok := lane.popForWrite()
	if !ok {
		t.Fatal("expected marker")
	}
	var mp eventFrameParams
	if err := json.Unmarshal(marker.Params, &mp); err != nil {
		t.Fatal(err)
	}
	if mp.Event != "events_lost" {
		t.Errorf("marker event = %q", mp.Event)
	}
	if mp.Seq != nil {
		t.Error("marker must be out-of-band (no seq)")
	}
	if len(mp.Context) == 0 {
		t.Error("marker must carry context")
	}
	if string(mp.Payload) != `{"lost":3}` {
		t.Errorf("marker payload = %s", mp.Payload)
	}

	// The surviving events are the NEWEST (oldest shed): first is n=3.
	first, ok := lane.popForWrite()
	if !ok {
		t.Fatal("expected event")
	}
	var fp eventFrameParams
	_ = json.Unmarshal(first.Params, &fp)
	if string(fp.Payload) != `{"n":3}` {
		t.Errorf("first surviving payload = %s, want {\"n\":3}", fp.Payload)
	}
}

func TestObserveLaneNoMarkerWhenNoLoss(t *testing.T) {
	lane := newObserveLane()
	ctx := json.RawMessage(`{"token":"e","tier":"event"}`)
	lane.push("turn_start", ctx, json.RawMessage(`{}`))
	f, ok := lane.popForWrite()
	if !ok {
		t.Fatal("expected event")
	}
	var fp eventFrameParams
	_ = json.Unmarshal(f.Params, &fp)
	if fp.Event != "turn_start" {
		t.Errorf("event = %q", fp.Event)
	}
	if _, ok := lane.popForWrite(); ok {
		t.Error("expected drained")
	}
}

func TestDefaultHandlerAnswersPingOnly(t *testing.T) {
	h := DefaultInboundHandler{}
	if _, err := h.HandleRequest(MethodPing, nil); err != nil {
		t.Errorf("ping: %v", err)
	}
	if _, err := h.HandleRequest("session/send_message", nil); err == nil || err.Code != CodeMethodNotFound {
		t.Errorf("expected MethodNotFound, got %v", err)
	}
}

// ---- subprocess hardening: env scrubbing, exhaustively ----

func TestBuildChildEnvScrubsAmbientSecretsKeepsAllowlistAndExplicit(t *testing.T) {
	// A fake host env: two allow-listed vars + secrets that must NOT pass.
	host := map[string]string{
		"PATH":                  "/usr/bin:/bin",
		"HOME":                  "/home/tester",
		"AWS_SECRET_ACCESS_KEY": "super-secret",
		"GITHUB_TOKEN":          "ghp_leak",
		"SOME_RANDOM_VAR":       "x",
	}
	lookup := func(k string) (string, bool) { v, ok := host[k]; return v, ok }

	env := envMap(buildChildEnv(lookup, map[string]string{"SEP_PROTO": "1"}))

	// Allow-listed launch essentials pass through.
	if env["PATH"] != "/usr/bin:/bin" {
		t.Errorf("PATH = %q want /usr/bin:/bin", env["PATH"])
	}
	if env["HOME"] != "/home/tester" {
		t.Errorf("HOME = %q want /home/tester", env["HOME"])
	}
	// The manifest's explicit (SEP-protocol) var passes through.
	if env["SEP_PROTO"] != "1" {
		t.Errorf("SEP_PROTO = %q want 1", env["SEP_PROTO"])
	}
	// The lethal-trifecta concern: ambient secrets are SCRUBBED.
	for _, k := range []string{"AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "SOME_RANDOM_VAR"} {
		if _, ok := env[k]; ok {
			t.Errorf("%s must not inherit into the child env", k)
		}
	}
}

func TestBuildChildEnvExplicitOverridesPassthrough(t *testing.T) {
	host := map[string]string{"PATH": "/host/path"}
	lookup := func(k string) (string, bool) { v, ok := host[k]; return v, ok }
	env := envMap(buildChildEnv(lookup, map[string]string{"PATH": "/ext/path"}))
	if env["PATH"] != "/ext/path" {
		t.Errorf("manifest env must win on collision: PATH = %q", env["PATH"])
	}
}

// envMap turns the KEY=VALUE slice exec.Cmd wants back into a map for assertions.
func envMap(kvs []string) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}
