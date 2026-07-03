package extension

import (
	"encoding/json"
	"strconv"
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
