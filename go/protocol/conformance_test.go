package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// specDir locates the repo's spec/ directory relative to this package (go/protocol).
func specDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "spec"))
	if err != nil {
		t.Fatalf("resolve spec dir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("spec dir not found at %s: %v", dir, err)
	}
	return dir
}

type fixture struct {
	SchemaRef   string          `json:"$schema_ref"`
	Description string          `json:"description"`
	Instance    json.RawMessage `json:"instance"`
}

func loadFixtures(t *testing.T, dir string) map[string]fixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "conformance", "fixtures.json"))
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	out := map[string]fixture{}
	for name, body := range all {
		if strings.HasPrefix(name, "$") {
			continue
		}
		var f fixture
		if err := json.Unmarshal(body, &f); err != nil {
			t.Fatalf("parse fixture %s: %v", name, err)
		}
		out[name] = f
	}
	return out
}

// TestFixturesValidateAgainstSchema runs every conformance fixture through the
// jsonschema validator against the schema it declares.
func TestFixturesValidateAgainstSchema(t *testing.T) {
	dir := specDir(t)
	v, err := NewValidator(dir)
	if err != nil {
		t.Fatalf("load validator: %v", err)
	}
	fixtures := loadFixtures(t, dir)
	if len(fixtures) == 0 {
		t.Fatal("no fixtures loaded")
	}

	for name, f := range fixtures {
		t.Run(name, func(t *testing.T) {
			inst, err := jsonValue(f.Instance)
			if err != nil {
				t.Fatalf("decode instance: %v", err)
			}
			if err := v.ValidateRef(f.SchemaRef, inst); err != nil {
				t.Fatalf("%s (%s) failed validation: %v", name, f.SchemaRef, err)
			}
		})
	}
}

// TestValidatorRejectsMutatedFixture asserts the validator actually rejects bad data.
func TestValidatorRejectsMutatedFixture(t *testing.T) {
	dir := specDir(t)
	v, err := NewValidator(dir)
	if err != nil {
		t.Fatalf("load validator: %v", err)
	}
	fixtures := loadFixtures(t, dir)
	f, ok := fixtures["stream_chunk_event"]
	if !ok {
		t.Fatal("stream_chunk_event fixture missing")
	}
	inst, err := jsonValue(f.Instance)
	if err != nil {
		t.Fatalf("decode instance: %v", err)
	}
	m := inst.(map[string]any)
	m["type"] = "not_a_real_event" // violates the const discriminator
	if err := v.ValidateRef(f.SchemaRef, m); err == nil {
		t.Fatal("expected validation failure for mutated fixture, got nil")
	}
}

// TestFixturesRoundTripIntoGoTypes asserts every fixture unmarshals into the right
// generated Go type and re-marshals without losing required fields. This catches
// json-tag drift independently of the schema validator.
func TestFixturesRoundTripIntoGoTypes(t *testing.T) {
	dir := specDir(t)
	fixtures := loadFixtures(t, dir)

	check := func(t *testing.T, raw json.RawMessage, target any, requiredKeys ...string) {
		t.Helper()
		if err := json.Unmarshal(raw, target); err != nil {
			t.Fatalf("unmarshal into %T: %v", target, err)
		}
		out, err := json.Marshal(target)
		if err != nil {
			t.Fatalf("re-marshal %T: %v", target, err)
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("re-parse %T: %v", target, err)
		}
		for _, k := range requiredKeys {
			if _, ok := m[k]; !ok {
				t.Errorf("round-trip of %T dropped required key %q", target, k)
			}
		}
	}

	t.Run("create_session_request", func(t *testing.T) {
		var req CreateConversationSessionRequest
		check(t, fixtures["create_session_request"].Instance, &req, "action", "agentId")
		if req.Action != string(ActionCreateConversationSession) {
			t.Errorf("action = %q", req.Action)
		}
		if req.AgentID != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("agentId = %q", req.AgentID)
		}
		if req.Metadata["planTier"] != "pro" {
			t.Errorf("metadata.planTier = %v", req.Metadata["planTier"])
		}
	})

	t.Run("create_session_response", func(t *testing.T) {
		var resp CreateConversationSessionResponse
		check(t, fixtures["create_session_response"].Instance, &resp,
			"sessionId", "conversationId", "agentId", "agentName", "userParticipantId", "agentParticipantId")
		if resp.AgentName != "Aria" {
			t.Errorf("agentName = %q", resp.AgentName)
		}
	})

	t.Run("send_message_request", func(t *testing.T) {
		var req SendMessageRequest
		check(t, fixtures["send_message_request"].Instance, &req, "action", "sessionId", "message")
		if !req.Stream {
			t.Errorf("stream = %v, want true", req.Stream)
		}
	})

	t.Run("stream_chunk_event", func(t *testing.T) {
		var ev StreamChunk
		check(t, fixtures["stream_chunk_event"].Instance, &ev, "type", "data")
		if ev.Node == nil || *ev.Node != "knowledge_search" {
			t.Errorf("node = %v", ev.Node)
		}
		if ev.Data.Node != "knowledge_search" {
			t.Errorf("data.node = %q", ev.Data.Node)
		}
	})

	t.Run("eventual_response_event", func(t *testing.T) {
		var ev EventualResponse
		check(t, fixtures["eventual_response_event"].Instance, &ev, "type", "data")
		// The protocol's awkward nested data.data: top-level data → inner data → messageId.
		if ev.Data.Data.MessageID != "66666666-6666-6666-6666-666666666666" {
			t.Errorf("data.data.messageId = %q", ev.Data.Data.MessageID)
		}
		if ev.Data.Status != 200 {
			t.Errorf("data.status = %d", ev.Data.Status)
		}
	})
}

// jsonValue decodes raw JSON into the generic any tree the validator expects.
func jsonValue(raw json.RawMessage) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}
