package core

import (
	"strings"
	"testing"
)

func msg(role, content string) ChatMessage {
	return ChatMessage{Role: role, Content: content}
}

func sumTokens(ms []ChatMessage) int {
	total := 0
	for _, m := range ms {
		total += estimateTokens(m)
	}
	return total
}

func TestCompactUnderBudgetUnchanged(t *testing.T) {
	ms := []ChatMessage{msg("system", "sys"), msg("user", "hi"), msg("assistant", "hello")}
	out := compact(ms, 8000)
	if len(out) != len(ms) {
		t.Fatalf("want %d messages, got %d", len(ms), len(out))
	}
}

func TestCompactDisabledWhenNonPositive(t *testing.T) {
	ms := []ChatMessage{msg("user", strings.Repeat("x", 10000))}
	out := compact(ms, 0)
	if len(out) != 1 {
		t.Fatalf("compaction should be disabled at budget 0")
	}
}

func TestCompactDropsOldestKeepsSystemAndRecent(t *testing.T) {
	big := strings.Repeat("word ", 200)
	ms := []ChatMessage{
		msg("system", "you are helpful"),
		msg("user", "OLDEST "+big),
		msg("assistant", "old reply "+big),
		msg("user", "MIDDLE "+big),
		msg("assistant", "mid reply "+big),
		msg("user", "NEWEST question"),
	}
	out := compact(ms, 400)
	if out[0].Role != "system" {
		t.Fatalf("system message must be kept first, got %q", out[0].Role)
	}
	var joined string
	for _, m := range out {
		joined += m.Content + " "
	}
	if !strings.Contains(joined, "NEWEST question") {
		t.Fatalf("newest message should survive; got %q", joined)
	}
	if strings.Contains(joined, "OLDEST") {
		t.Fatalf("oldest message should be dropped; got %q", joined)
	}
	if sumTokens(out) > 400 {
		t.Fatalf("compacted result %d tokens exceeds budget 400", sumTokens(out))
	}
}

func TestCompactNeverStartsOnOrphanTool(t *testing.T) {
	big := strings.Repeat("token ", 300)
	ms := []ChatMessage{
		msg("system", "sys"),
		msg("user", "q "+big),
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "t", Arguments: "{}"}}},
		{Role: "tool", ToolCallID: "c1", Content: "result " + big},
		msg("assistant", "final answer"),
	}
	out := compact(ms, 200)
	var firstNonSystem *ChatMessage
	for i := range out {
		if out[i].Role != "system" {
			firstNonSystem = &out[i]
			break
		}
	}
	if firstNonSystem == nil {
		t.Fatalf("expected at least one non-system message kept")
	}
	if firstNonSystem.Role == "tool" {
		t.Fatalf("kept window must not start on an orphan tool message")
	}
}
