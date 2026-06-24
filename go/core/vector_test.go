package core

import (
	"math"
	"strings"
	"testing"
)

func TestHashEmbedderDeterministicAndNormalized(t *testing.T) {
	emb := NewHashEmbedder(64)
	a := emb.Embed("return policy details")
	b := emb.Embed("return policy details")
	if len(a) != 64 {
		t.Fatalf("dim = %d", len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("not deterministic at %d", i)
		}
	}
	var norm float64
	for _, v := range a {
		norm += v * v
	}
	if math.Abs(math.Sqrt(norm)-1.0) > 1e-9 {
		t.Fatalf("not L2-normalized: %v", math.Sqrt(norm))
	}
}

func TestVectorKnowledgeRetrievesMostSimilar(t *testing.T) {
	kb := NewVectorKnowledge(NewHashEmbedder(256))
	kb.Ingest("Our return policy allows refunds within 30 days.", "returns.md")
	kb.Ingest("The office is open Monday through Friday.", "hours.md")
	hits := kb.Query("how do refunds and returns work?", 1)
	if len(hits) != 1 || hits[0].Source != "returns.md" || hits[0].Score <= 0 {
		t.Fatalf("expected returns.md top hit: %+v", hits)
	}
}

func TestVectorKnowledgeEmptyStore(t *testing.T) {
	if got := NewVectorKnowledge(nil).Query("anything", 4); got != nil {
		t.Fatalf("empty store should return nil, got %+v", got)
	}
}

func TestAgentAcceptsVectorKnowledge(t *testing.T) {
	kb := NewVectorKnowledge(nil)
	kb.Ingest("Gift wrapping costs 4.99 per item.", "wrapping.md")
	kb.Ingest("Returns are accepted within 30 days.", "returns.md")

	agent := NewSmoothAgent(&fakeClient{}, AgentOptions{Instructions: "support", Knowledge: kb, KnowledgeTopK: 1})
	system := agent.buildSystem("how much is gift wrapping?")
	if !strings.Contains(system, "[wrapping.md]") {
		t.Fatalf("vector knowledge should be injected: %q", system)
	}
}
