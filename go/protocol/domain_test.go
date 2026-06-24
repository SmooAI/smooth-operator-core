package protocol

import (
	"encoding/json"
	"testing"
)

// TestDomainRoundTrip exercises the domain types: a Participant (ai-agent), a
// Message (inbound), and a Session (with threadId) marshal and unmarshal without
// losing fields or mangling enum values.
func TestParticipantRoundTrip(t *testing.T) {
	src := `{
		"id": "44444444-4444-4444-4444-444444444444",
		"conversationId": "33333333-3333-3333-3333-333333333333",
		"organizationId": "99999999-9999-9999-9999-999999999999",
		"type": "ai-agent",
		"name": "Aria",
		"email": null,
		"createdAt": "2026-06-01T12:00:00Z",
		"updatedAt": "2026-06-01T12:05:00Z"
	}`

	var p Participant
	if err := json.Unmarshal([]byte(src), &p); err != nil {
		t.Fatalf("unmarshal participant: %v", err)
	}
	if p.Type != ParticipantTypeAiAgent {
		t.Errorf("type = %q, want %q", p.Type, ParticipantTypeAiAgent)
	}
	if p.Name != "Aria" {
		t.Errorf("name = %q", p.Name)
	}

	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal participant: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if m["type"] != "ai-agent" {
		t.Errorf("round-trip type = %v", m["type"])
	}
	for _, k := range []string{"id", "conversationId", "organizationId", "type", "name", "createdAt", "updatedAt"} {
		if _, ok := m[k]; !ok {
			t.Errorf("dropped required key %q", k)
		}
	}
}

func TestMessageInboundRoundTrip(t *testing.T) {
	src := `{
		"id": "66666666-6666-6666-6666-666666666666",
		"direction": "inbound",
		"content": { "text": "What is the status of my last order?", "items": [{"type": "text", "text": "What is the status of my last order?"}] },
		"from": { "id": "44444444-4444-4444-4444-444444444444", "type": "user", "name": "Alice" },
		"createdAt": "2026-06-01T12:00:00Z"
	}`

	var msg Message
	if err := json.Unmarshal([]byte(src), &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if msg.Direction != MessageDirectionInbound {
		t.Errorf("direction = %q, want %q", msg.Direction, MessageDirectionInbound)
	}
	if msg.Content.Text == nil || *msg.Content.Text == "" {
		t.Errorf("content.text empty")
	}
	if len(msg.Content.Items) != 1 || msg.Content.Items[0].Type != ContentItemTypeText {
		t.Errorf("content.items = %+v", msg.Content.Items)
	}
	if msg.From == nil || msg.From.Type != "user" {
		t.Errorf("from = %+v", msg.From)
	}
}

func TestSessionThreadIDRoundTrip(t *testing.T) {
	src := `{
		"sessionId": "22222222-2222-2222-2222-222222222222",
		"conversationId": "33333333-3333-3333-3333-333333333333",
		"agentId": "11111111-1111-1111-1111-111111111111",
		"agentName": "Aria",
		"userParticipantId": "44444444-4444-4444-4444-444444444444",
		"agentParticipantId": "55555555-5555-5555-5555-555555555555",
		"threadId": "thread-abc-123",
		"status": "active"
	}`

	var s Session
	if err := json.Unmarshal([]byte(src), &s); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}
	if s.ThreadID != "thread-abc-123" {
		t.Errorf("threadId = %q", s.ThreadID)
	}
	if s.Status == nil || *s.Status != SessionStatusActive {
		t.Errorf("status = %v, want %q", s.Status, SessionStatusActive)
	}

	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if m["threadId"] != "thread-abc-123" {
		t.Errorf("round-trip threadId = %v", m["threadId"])
	}
}

// TestParseServerEventDiscrimination asserts ParseServerEvent populates the common
// envelope fields and rejects unknown types.
func TestParseServerEventDiscrimination(t *testing.T) {
	frame := []byte(`{"type":"stream_token","requestId":"req-1","token":"Hi","data":{"requestId":"req-1","token":"Hi"}}`)
	ev, err := ParseServerEvent(frame)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.Type != EventStreamToken || ev.RequestID != "req-1" || ev.Token != "Hi" {
		t.Errorf("ev = %+v", ev)
	}
	tok, err := ev.AsStreamToken()
	if err != nil {
		t.Fatalf("AsStreamToken: %v", err)
	}
	if tok.Data.Token != "Hi" {
		t.Errorf("data.token = %q", tok.Data.Token)
	}

	if _, err := ParseServerEvent([]byte(`{"type":"bogus"}`)); err == nil {
		t.Error("expected error for unknown event type")
	}
}
