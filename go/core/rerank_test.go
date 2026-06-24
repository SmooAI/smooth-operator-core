package core

import (
	"strings"
	"testing"
)

func hit(content string) KnowledgeHit {
	return KnowledgeHit{Content: content, Source: "s", Score: 0}
}

func TestNoopRerankerPassthrough(t *testing.T) {
	hits := []KnowledgeHit{hit("a"), hit("b")}
	out := NoopReranker{}.Rerank("q", hits)
	if len(out) != 2 || out[0].Content != "a" {
		t.Fatalf("noop should be passthrough: %+v", out)
	}
}

func TestLexicalRerankerPrefersConcise(t *testing.T) {
	concise := hit("return policy")
	verbose := hit("return " + strings.Repeat("filler ", 60) + "policy")
	out := LexicalReranker{}.Rerank("return policy", []KnowledgeHit{verbose, concise})
	if out[0].Content != concise.Content {
		t.Fatalf("concise doc should rank first: %+v", out)
	}
}

func TestLexicalRerankerPrefersHigherCoverage(t *testing.T) {
	coversTwo := hit("return and policy details")
	coversOne := hit("return shipping details")
	out := LexicalReranker{}.Rerank("return policy", []KnowledgeHit{coversOne, coversTwo})
	if out[0].Content != coversTwo.Content {
		t.Fatalf("higher-coverage doc should rank first: %+v", out)
	}
}

func TestLexicalRerankerEmptyQueryPassthrough(t *testing.T) {
	hits := []KnowledgeHit{hit("a"), hit("b")}
	out := LexicalReranker{}.Rerank("", hits)
	if out[0].Content != "a" {
		t.Fatalf("empty query should be passthrough")
	}
}

func TestRerankerAppliedInBuildSystem(t *testing.T) {
	kb := &InMemoryKnowledge{}
	kb.Ingest("return "+strings.Repeat("filler ", 60)+"policy", "long.md")
	kb.Ingest("return policy", "short.md")

	agent := NewSmoothAgent(&fakeClient{}, AgentOptions{
		Instructions:        "support",
		Knowledge:           kb,
		KnowledgeTopK:       1,
		KnowledgeCandidateK: 2,
		Reranker:            LexicalReranker{},
	})
	system := agent.buildSystem("return policy")
	if !strings.Contains(system, "[short.md]") {
		t.Fatalf("reranked top-1 (short.md) should be injected: %q", system)
	}
	if strings.Contains(system, "[long.md]") {
		t.Fatalf("long.md should have been reranked out: %q", system)
	}
}
