package core

import (
	"strings"
	"testing"
)

func TestMemoryRememberAndRecall(t *testing.T) {
	m := &InMemoryMemory{}
	m.Remember("The user's name is Dana.")
	m.Remember("The user prefers metric units.")
	m.Remember("Gift wrapping costs 4.99.")
	recalled := m.Recall("what units does the user prefer?", 1)
	if len(recalled) != 1 || !strings.Contains(recalled[0].Text, "metric") {
		t.Fatalf("recall failed: %+v", recalled)
	}
}

func TestMemoryRecallNoOverlap(t *testing.T) {
	m := &InMemoryMemory{}
	m.Remember("The sky is blue.")
	if got := m.Recall("quarterly revenue forecast", 4); len(got) != 0 {
		t.Fatalf("expected no recall, got %+v", got)
	}
}

func TestMemoryIgnoresBlank(t *testing.T) {
	m := &InMemoryMemory{}
	m.Remember("   ")
	if got := m.Recall("anything", 4); len(got) != 0 {
		t.Fatalf("blank should be ignored, got %+v", got)
	}
}

func TestMemoryInjectedInBuildSystem(t *testing.T) {
	m := &InMemoryMemory{}
	m.Remember("The user's name is Dana.")
	m.Remember("Unrelated trivia about penguins.")

	agent := NewSmoothAgent(&fakeClient{}, AgentOptions{Instructions: "support", Memory: m})
	system := agent.buildSystem("do you remember my name?")
	if !strings.Contains(system, RecallHeader) || !strings.Contains(system, "Dana") {
		t.Fatalf("recalled memory should be injected: %q", system)
	}
	if strings.Contains(system, "penguins") {
		t.Fatalf("unrelated memory should not be recalled: %q", system)
	}
}

// ── cross-language recall contract (th-ffaeae) ───────────────────────────────
//
// The block below is reproduced byte-for-byte by the Rust reference and the C#,
// Python and TypeScript siblings. These tests mirror
// rust/smooth-operator-core/src/memory.rs so a drift shows up here, not months later
// when someone tries to write a shared conformance scenario.

func TestRecallBlockIsPinnedAcrossLanguages(t *testing.T) {
	got := RenderRecallBlock([]MemoryEntry{{Text: "brent prefers execution over questions", Type: MemoryUser, Relevance: 0.5}})
	want := "[Recalled memories]\n- (User, relevance=0.50): brent prefers execution over questions\n"
	if got != want {
		t.Fatalf("block =\n%q\nwant\n%q", got, want)
	}
}

// A Project/Reference memory names something in a moving codebase, so the model is
// told to verify it. A User/Feedback one describes the person and does not go stale —
// emitting the note there would train the model to skip it.
func TestRecallBlockFreshnessNoteOnlyWhenTimeSensitive(t *testing.T) {
	got := RenderRecallBlock([]MemoryEntry{{Text: "the retry lives in fetch.rs", Type: MemoryProject, Relevance: 1}})
	want := RecallHeader + "\n" + RecallFreshnessNote + "\n- (Project, relevance=1.00): the retry lives in fetch.rs\n"
	if got != want {
		t.Fatalf("block =\n%q\nwant\n%q", got, want)
	}
	durable := RenderRecallBlock([]MemoryEntry{{Text: "prefers dark mode", Type: MemoryUser, Relevance: 1}})
	if strings.Contains(durable, "Note:") {
		t.Fatalf("a durable memory must not carry the freshness note: %q", durable)
	}
}

// A bare header would spend context telling the model it remembered nothing.
func TestEmptyRecallRendersNoBlock(t *testing.T) {
	if got := RenderRecallBlock(nil); got != "" {
		t.Fatalf("empty recall rendered %q", got)
	}
}

// Normalised to 0-1 so it is comparable between entries AND between languages — a raw
// overlap count is neither, and it is rendered into the prompt.
func TestRelevanceIsAFractionOfQueryTokens(t *testing.T) {
	if got := RelevanceScore("watchlist on marvin today", "the watchlist lives on smoo-hub"); got != 0.5 {
		t.Fatalf("relevance = %v, want 0.5", got)
	}
	if got := RelevanceScore("", "anything"); got != 0 {
		t.Fatalf("empty query scored %v", got)
	}
}

// Scoring used to split on whitespace only, so "do you remember my name?" scored 0
// against "the user's name is Dana" — the trailing '?' made `name?` fail — and the
// memory was silently never recalled.
func TestPunctuationDoesNotDefeatAMatch(t *testing.T) {
	if got := RelevanceScore("do you remember my name?", "The user's name is Dana."); got <= 0 {
		t.Fatalf("punctuation defeated the match: %v", got)
	}
	if got := RelevanceScore("watchlist!", "the watchlist lives here"); got != 1 {
		t.Fatalf("relevance = %v, want 1", got)
	}
}

func TestRecallPopulatesRelevanceAndType(t *testing.T) {
	m := &InMemoryMemory{}
	m.RememberTyped("the watchlist lives on smoo-hub", MemoryProject)
	hits := m.Recall("watchlist on marvin today", MemoryTopK)
	if len(hits) != 1 {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].Relevance != 0.5 || hits[0].Type != MemoryProject {
		t.Fatalf("hit = %+v, want relevance 0.5 and Project", hits[0])
	}
}
