package temporal

import (
	"context"
	"testing"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// End-to-end integration test against a real (ephemeral) Temporal dev server: stand
// up a worker, execute the scaffold HealthWorkflow through it, and assert the
// activity result came back through the workflow. Self-skips when no Temporal is
// reachable (see devServerClient). The Go mirror of the Rust
// health_workflow_e2e.rs.
func TestHealthWorkflow_RunsEndToEnd(t *testing.T) {
	c := devServerClient(t)

	const taskQueue = "smooth-operator-temporal-health-test"
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(HealthWorkflow)
	// Health needs no model/tools; a bare activities struct suffices for HealthEcho.
	w.RegisterActivity(NewAgentTurnActivities(nil, "", nil))
	if err := w.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	defer w.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "health-e2e-1",
		TaskQueue: taskQueue,
	}, HealthWorkflow, "ping")
	if err != nil {
		t.Fatalf("execute workflow: %v", err)
	}

	var result string
	if err := run.Get(ctx, &result); err != nil {
		t.Fatalf("get result: %v", err)
	}
	if result != "smooth-operator-temporal ok: ping" {
		t.Fatalf("unexpected health result: %q", result)
	}
}
