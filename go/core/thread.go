package core

// Conversation threads — carry message history across SmoothAgent.Run calls.
//
// Phase-2 sibling of the C# SmoothAgentThread (dotnet/core) and the reference
// engine's persisted Conversation. A SmoothAgentThread is the in-memory handle you
// hold per user conversation and pass to each run: the agent seeds the turn from the
// thread's messages, runs, and appends this turn's user/assistant/tool messages back
// to it, so the next turn has the full context. The system prompt is supplied per-run
// from instructions/knowledge/memory and is never stored on the thread.
//
// This complements checkpointing (checkpoint.go): a checkpoint persists a conversation
// to a store keyed by id; a thread is the live in-memory object you pass between runs.
// The thread's ID is the natural key to checkpoint under.

import (
	"strings"

	"github.com/google/uuid"
)

// SmoothAgentThread is a conversation thread: a stable id plus the ordered
// non-system messages so far.
type SmoothAgentThread struct {
	// ID is the stable id for this conversation (the key checkpoints store under).
	ID string
	// messages is the ordered history, oldest first, never including a system message.
	messages []ChatMessage
}

// NewThread creates a fresh thread with an auto-generated id.
func NewThread() *SmoothAgentThread {
	return &SmoothAgentThread{ID: strings.ReplaceAll(uuid.NewString(), "-", "")}
}

// NewThreadWithID resumes a thread under a known id (e.g. one recovered from a
// checkpoint). An empty id falls back to a fresh auto-generated one.
func NewThreadWithID(id string) *SmoothAgentThread {
	if id == "" {
		return NewThread()
	}
	return &SmoothAgentThread{ID: id}
}

// Messages returns the accumulated history, oldest first (no system prompt).
func (t *SmoothAgentThread) Messages() []ChatMessage { return t.messages }

// Len returns the number of messages currently in the thread.
func (t *SmoothAgentThread) Len() int { return len(t.messages) }

// Add appends one message, skipping any system message (rebuilt per-run).
func (t *SmoothAgentThread) Add(m ChatMessage) {
	if m.Role == "system" {
		return
	}
	t.messages = append(t.messages, m)
}

// Extend appends several messages, skipping any system messages.
func (t *SmoothAgentThread) Extend(msgs []ChatMessage) {
	for _, m := range msgs {
		t.Add(m)
	}
}
