package core

// Conversation checkpointing — persist a turn's state so it can resume.
//
// Phase-1 sibling of the reference engines' checkpointing. A CheckpointStore
// saves and loads the conversation (the non-system messages) keyed by a
// conversation id, so a later turn — even in a new process — can pick up where
// the last left off. InMemoryCheckpointStore is the zero-dependency default.

// Checkpoint is a saved conversation: its id and the non-system messages so far.
type Checkpoint struct {
	ConversationID string
	Messages       []ChatMessage
}

// CheckpointStore persists and restores conversations by id.
type CheckpointStore interface {
	Save(cp Checkpoint)
	Load(conversationID string) (Checkpoint, bool)
}

// InMemoryCheckpointStore is a process-local store backed by a map.
type InMemoryCheckpointStore struct {
	store map[string]Checkpoint
}

// NewInMemoryCheckpointStore creates an empty in-memory store.
func NewInMemoryCheckpointStore() *InMemoryCheckpointStore {
	return &InMemoryCheckpointStore{store: map[string]Checkpoint{}}
}

// Save persists a copy of the checkpoint's messages.
func (s *InMemoryCheckpointStore) Save(cp Checkpoint) {
	msgs := make([]ChatMessage, len(cp.Messages))
	copy(msgs, cp.Messages)
	s.store[cp.ConversationID] = Checkpoint{ConversationID: cp.ConversationID, Messages: msgs}
}

// Load returns the checkpoint for an id, and whether one was found.
func (s *InMemoryCheckpointStore) Load(conversationID string) (Checkpoint, bool) {
	cp, ok := s.store[conversationID]
	return cp, ok
}
