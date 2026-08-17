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

// echoTool is a real tool the HITL gate lets run (or blocks): it echoes its `text`
// argument back.
func echoTool() core.Tool {
	return core.FuncTool{
		ToolName: "echo",
		Desc:     "Echoes input back",
		Params: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []any{"text"},
		},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			text, _ := args["text"].(string)
			return text, nil
		},
	}
}

// End-to-end test of DURABLE human-in-the-loop via Temporal signals. A turn whose
// model calls an approval-gated tool blocks the workflow until an approve_tool /
// deny_tool signal names that tool call. Two turns run against an ephemeral dev
// server: one APPROVED (the tool runs), one DENIED (the tool is skipped with an
// error result the model sees). This is the durable HITL unlock — the block is
// recorded in workflow history, so it survives restarts. The Go mirror of the Rust
// hitl_signals_e2e.rs.
func TestHITLGate_ApprovesAndDeniesViaSignals(t *testing.T) {
	c := devServerClient(t)

	// One mock model, scripted FIFO across BOTH (sequential) turns:
	//   turn 1 (approved): echo tool call, then a wrap-up reply
	//   turn 2 (denied):   echo tool call, then a wrap-up reply
	mock := core.NewMockLlmProvider().
		PushToolCall("call-approve", "echo", `{"text":"ran-after-approval"}`).
		PushText("done after approval").
		PushToolCall("call-deny", "echo", `{"text":"should-not-run"}`).
		PushText("done after denial")
	acts := NewAgentTurnActivities(mock, "", []core.Tool{echoTool()})

	const taskQueue = "smooth-operator-temporal-hitl-test"
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(AgentTurnWorkflow)
	w.RegisterActivity(acts)
	if err := w.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	defer w.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	input := func(id string) AgentTurnInput {
		return AgentTurnInput{
			SystemPrompt:          "You are a gated agent",
			UserMessage:           "use echo (" + id + ")",
			MaxIterations:         5,
			ApprovalRequiredTools: []string{"echo"},
		}
	}

	// --- Turn 1: APPROVE ---
	approveRun, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "hitl-approve",
		TaskQueue: taskQueue,
	}, AgentTurnWorkflow, input("approve"))
	if err != nil {
		t.Fatalf("execute approve workflow: %v", err)
	}
	// The tool-call id is deterministic from the mock script; the signal buffers, so
	// the gate sees it approved when it checks.
	if err := c.SignalWorkflow(ctx, approveRun.GetID(), approveRun.GetRunID(), SignalApproveTool, "call-approve"); err != nil {
		t.Fatalf("signal approve: %v", err)
	}
	var approved []core.ChatMessage
	if err := approveRun.Get(ctx, &approved); err != nil {
		t.Fatalf("get approved result: %v", err)
	}

	// --- Turn 2: DENY ---
	denyRun, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "hitl-deny",
		TaskQueue: taskQueue,
	}, AgentTurnWorkflow, input("deny"))
	if err != nil {
		t.Fatalf("execute deny workflow: %v", err)
	}
	if err := c.SignalWorkflow(ctx, denyRun.GetID(), denyRun.GetRunID(), SignalDenyTool, "call-deny"); err != nil {
		t.Fatalf("signal deny: %v", err)
	}
	var denied []core.ChatMessage
	if err := denyRun.Get(ctx, &denied); err != nil {
		t.Fatalf("get denied result: %v", err)
	}

	// Approved turn: the gated tool actually ran, its real result is in the
	// conversation, and the turn finished.
	approvedTools := toolMessages(approved)
	if len(approvedTools) != 1 {
		t.Fatalf("approved tool message count = %d, want 1: %+v", len(approvedTools), approved)
	}
	if approvedTools[0].Content != "ran-after-approval" {
		t.Fatalf("approved tool result = %q, want the echo payload", approvedTools[0].Content)
	}
	if got := lastAssistantContent(approved); got != "done after approval" {
		t.Fatalf("approved last assistant = %q", got)
	}

	// Denied turn: the tool NEVER ran — the tool message is a denial error, not the
	// echo payload — and the model still got to wrap up.
	deniedTools := toolMessages(denied)
	if len(deniedTools) != 1 {
		t.Fatalf("denied tool message count = %d, want 1: %+v", len(deniedTools), denied)
	}
	if !strings.Contains(deniedTools[0].Content, "denied by human approval") {
		t.Fatalf("denied tool result = %q, want a denial error", deniedTools[0].Content)
	}
	if deniedTools[0].Content == "should-not-run" {
		t.Fatal("denied tool actually ran — the echo payload is present")
	}
	if got := lastAssistantContent(denied); got != "done after denial" {
		t.Fatalf("denied last assistant = %q", got)
	}
}
