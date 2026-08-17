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

// End-to-end test of a DURABLE TIMER — an agent that pauses itself on a Temporal
// timer mid-turn, then resumes. The model calls the configured `wait` tool; the
// workflow sleeps on workflow.Sleep (recorded in history, so it survives restarts
// and can span days) and then continues the turn. A short (1s) real timer proves
// the durable pause without depending on time-skipping. The Go mirror of the Rust
// durable_timer_e2e.rs.
func TestDurableWaitTool_SleepsThenResumes(t *testing.T) {
	c := devServerClient(t)

	// The model asks to wait 1 second, then wraps up. No tools registered — the
	// `wait` tool is handled by the workflow, not the tool registry/activity.
	mock := core.NewMockLlmProvider().
		PushToolCall("call-wait", "wait", `{"seconds":1}`).
		PushText("resumed after the timer")
	acts := NewAgentTurnActivities(mock, "", nil)

	const taskQueue = "smooth-operator-temporal-timer-test"
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(AgentTurnWorkflow)
	w.RegisterActivity(acts)
	if err := w.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	defer w.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	started := time.Now()
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "durable-timer-1",
		TaskQueue: taskQueue,
	}, AgentTurnWorkflow, AgentTurnInput{
		SystemPrompt:  "You are a self-pacing agent",
		UserMessage:   "wait a moment, then answer",
		MaxIterations: 5,
		WaitTool:      "wait",
	})
	if err != nil {
		t.Fatalf("execute workflow: %v", err)
	}

	var conversation []core.ChatMessage
	if err := run.Get(ctx, &conversation); err != nil {
		t.Fatalf("get result: %v", err)
	}
	elapsed := time.Since(started)

	// The wait tool produced a durable-timer result, and the turn resumed after it.
	tools := toolMessages(conversation)
	if len(tools) != 1 {
		t.Fatalf("tool message count = %d, want 1: %+v", len(tools), conversation)
	}
	if !strings.Contains(tools[0].Content, "durable timer") {
		t.Fatalf("unexpected wait result: %q", tools[0].Content)
	}
	if got := lastAssistantContent(conversation); got != "resumed after the timer" {
		t.Fatalf("last assistant content = %q, want the post-timer reply", got)
	}
	// It actually waited (the 1s timer elapsed), not skipped instantly.
	if elapsed < 900*time.Millisecond {
		t.Fatalf("turn returned too fast to have honored the timer: %v", elapsed)
	}
}
