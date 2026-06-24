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
	if !strings.Contains(system, "Relevant memory") || !strings.Contains(system, "Dana") {
		t.Fatalf("recalled memory should be injected: %q", system)
	}
	if strings.Contains(system, "penguins") {
		t.Fatalf("unrelated memory should not be recalled: %q", system)
	}
}
