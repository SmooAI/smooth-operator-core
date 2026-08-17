package temporal

import (
	"context"
	"strings"
	"testing"
	"time"

	core "github.com/SmooAI/smooth-operator-core/go/core"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// End-to-end test of a REAL agent turn running through AgentTurnWorkflow against an
// ephemeral Temporal dev server. The model call is backed by a MockLlmProvider, so
// the whole per-step path is exercised: workflow → ModelCall activity (mock model)
// → engine DriveTurn → returned conversation. This is the proof that the durable
// backend runs the same loop as the in-process path. The Go mirror of the Rust
// agent_turn_e2e.rs.
func TestAgentTurnWorkflow_RunsARealTurn(t *testing.T) {
	c := devServerClient(t)

	mock := core.NewMockLlmProvider().PushText("the durable answer is 42")
	acts := NewAgentTurnActivities(mock, "", nil)

	const taskQueue = "smooth-operator-temporal-agent-turn-test"
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(AgentTurnWorkflow)
	w.RegisterWorkflow(HealthWorkflow)
	w.RegisterActivity(acts)
	if err := w.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	defer w.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "agent-turn-e2e-1",
		TaskQueue: taskQueue,
	}, AgentTurnWorkflow, AgentTurnInput{
		SystemPrompt:  "You are a test agent",
		UserMessage:   "what is the durable answer?",
		MaxIterations: 5,
	})
	if err != nil {
		t.Fatalf("execute workflow: %v", err)
	}

	var conversation []core.ChatMessage
	if err := run.Get(ctx, &conversation); err != nil {
		t.Fatalf("get result: %v", err)
	}

	// The turn ran through the workflow: the mock's scripted reply is the final
	// assistant message, and the model was called exactly once (no tools).
	if got := lastAssistantContent(conversation); got != "the durable answer is 42" {
		t.Fatalf("last assistant content = %q, want the scripted reply", got)
	}
	if mock.CallCount() != 1 {
		t.Fatalf("model call count = %d, want 1", mock.CallCount())
	}
	// The user's message reached the model through the activity boundary.
	calls := mock.Calls()
	if len(calls) == 0 {
		t.Fatal("model received no calls")
	}
	var reachedModel bool
	for _, m := range calls[0].Messages {
		if strings.Contains(m.Content, "what is the durable answer?") {
			reachedModel = true
			break
		}
	}
	if !reachedModel {
		t.Fatalf("user message did not reach the model: %+v", calls[0].Messages)
	}
}
