package core

import (
	"context"
	"testing"
)

func TestCheckpointStoreRoundTrip(t *testing.T) {
	store := NewInMemoryCheckpointStore()
	if _, ok := store.Load("missing"); ok {
		t.Fatal("missing id should not be found")
	}
	store.Save(Checkpoint{ConversationID: "c1", Messages: []ChatMessage{{Role: "user", Content: "hi"}}})
	cp, ok := store.Load("c1")
	if !ok || len(cp.Messages) != 1 || cp.Messages[0].Content != "hi" {
		t.Fatalf("roundtrip failed: %+v ok=%v", cp, ok)
	}
}

func TestCheckpointPersistsAndResumes(t *testing.T) {
	store := NewInMemoryCheckpointStore()
	client := &fakeClient{scripted: []ChatResponse{{Content: "first answer"}, {Content: "second answer"}}}
	agent := NewSmoothAgent(client, AgentOptions{CheckpointStore: store, ConversationID: "conv-1"})

	if _, err := agent.Run(context.Background(), "hello", nil); err != nil {
		t.Fatal(err)
	}
	cp, ok := store.Load("conv-1")
	if !ok || len(cp.Messages) != 2 || cp.Messages[0].Content != "hello" || cp.Messages[1].Content != "first answer" {
		t.Fatalf("after turn 1: %+v ok=%v", cp, ok)
	}

	if _, err := agent.Run(context.Background(), "again", nil); err != nil {
		t.Fatal(err)
	}
	// The second model call should have seen the prior conversation.
	second := client.calls[1].Messages
	var sawHello, sawFirst, sawAgain bool
	for _, m := range second {
		switch m.Content {
		case "hello":
			sawHello = true
		case "first answer":
			sawFirst = true
		case "again":
			sawAgain = true
		}
	}
	if !sawHello || !sawFirst || !sawAgain {
		t.Fatalf("second call missing prior history: %+v", second)
	}
	cp2, _ := store.Load("conv-1")
	if len(cp2.Messages) != 4 {
		t.Fatalf("expected 4 accumulated messages, got %d", len(cp2.Messages))
	}
}

func TestNoCheckpointWhenConversationIDUnset(t *testing.T) {
	store := NewInMemoryCheckpointStore()
	client := &fakeClient{scripted: []ChatResponse{{Content: "hi"}}}
	agent := NewSmoothAgent(client, AgentOptions{CheckpointStore: store})
	if _, err := agent.Run(context.Background(), "hello", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Load("conv-1"); ok {
		t.Fatal("checkpoint should not be saved without a ConversationID")
	}
}
