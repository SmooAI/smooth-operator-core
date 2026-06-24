package core

import (
	"context"
	"strings"
	"testing"
)

// gateSpyTool records every invocation so a test can assert it never ran.
func gateSpyTool(invocations *[]map[string]any) FuncTool {
	return FuncTool{
		ToolName: "delete_record",
		Desc:     "Deletes a record (destructive).",
		Params:   map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []string{"id"}},
		Fn: func(_ context.Context, args map[string]any) (string, error) {
			*invocations = append(*invocations, args)
			id, _ := args["id"].(string)
			return "deleted record " + id, nil
		},
	}
}

func TestHumanResponseHelpers(t *testing.T) {
	if !Approve().IsApproved() {
		t.Fatal("Approve() should be approved")
	}
	d := Deny("nope")
	if d.IsApproved() || d.Decision != HumanDenied || d.Reason != "nope" {
		t.Fatalf("unexpected deny response: %+v", d)
	}
}

func TestApprovedToolExecutes(t *testing.T) {
	var invocations []map[string]any
	tool := gateSpyTool(&invocations)
	var seen []HumanApprovalRequest

	client := &fakeClient{scripted: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "delete_record", Arguments: `{"id":"42"}`}}},
		{Content: "done"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{
		Tools: []Tool{tool},
		HumanGate: func(_ context.Context, req HumanApprovalRequest) (HumanApprovalResponse, error) {
			seen = append(seen, req)
			return Approve(), nil
		},
		RequiresApproval: func(name string, _ map[string]any) bool { return name == "delete_record" },
	})

	res, err := agent.Run(context.Background(), "delete record 42", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" || res.ToolCalls != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	// The gate saw the right request, and the tool actually ran.
	if len(seen) != 1 || seen[0].ToolName != "delete_record" {
		t.Fatalf("gate not consulted correctly: %+v", seen)
	}
	if id, _ := seen[0].Arguments["id"].(string); id != "42" {
		t.Fatalf("request args not passed through: %+v", seen[0].Arguments)
	}
	if len(invocations) != 1 {
		t.Fatalf("tool should have run once, ran %d times", len(invocations))
	}
	// The successful tool result was fed back to the model.
	second := client.calls[1].Messages
	found := false
	for _, m := range second {
		if m.Role == "tool" && strings.Contains(m.Content, "deleted record 42") {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool result not fed back; messages=%+v", second)
	}
}

func TestDeniedToolDoesNotExecuteAndReasonReachesModel(t *testing.T) {
	var invocations []map[string]any
	tool := gateSpyTool(&invocations)

	client := &fakeClient{scripted: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "delete_record", Arguments: `{"id":"42"}`}}},
		{Content: "understood, I won't delete it"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{
		Tools: []Tool{tool},
		HumanGate: func(_ context.Context, _ HumanApprovalRequest) (HumanApprovalResponse, error) {
			return Deny("policy forbids deletes"), nil
		},
		RequiresApproval: func(name string, _ map[string]any) bool { return name == "delete_record" },
	})

	res, err := agent.Run(context.Background(), "delete record 42", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The tool never ran.
	if len(invocations) != 0 {
		t.Fatalf("denied tool should not run; ran %d times", len(invocations))
	}
	if res.Text != "understood, I won't delete it" {
		t.Fatalf("unexpected final text: %q", res.Text)
	}
	// The denial (with reason) was fed back to the model as the tool result.
	second := client.calls[1].Messages
	var denial string
	for _, m := range second {
		if m.Role == "tool" {
			denial = m.Content
		}
	}
	if !strings.Contains(denial, "Denied by human") || !strings.Contains(denial, "policy forbids deletes") {
		t.Fatalf("denial reason not fed back; got %q", denial)
	}
}

func TestNoGateConfiguredLeavesBehaviorUnchanged(t *testing.T) {
	var invocations []map[string]any
	tool := gateSpyTool(&invocations)

	client := &fakeClient{scripted: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "delete_record", Arguments: `{"id":"42"}`}}},
		{Content: "done"},
	}}
	// No HumanGate set — even though RequiresApproval matches, it is ignored.
	agent := NewSmoothAgent(client, AgentOptions{
		Tools:            []Tool{tool},
		RequiresApproval: func(string, map[string]any) bool { return true },
	})

	res, err := agent.Run(context.Background(), "delete record 42", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "done" || len(invocations) != 1 {
		t.Fatalf("expected unchanged behavior; result=%+v invocations=%d", res, len(invocations))
	}
}

func TestGateOnlyConsultsFlaggedTools(t *testing.T) {
	var invocations []map[string]any
	tool := gateSpyTool(&invocations)
	var consulted []string

	client := &fakeClient{scripted: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "delete_record", Arguments: `{"id":"7"}`}}},
		{Content: "done"},
	}}
	agent := NewSmoothAgent(client, AgentOptions{
		Tools: []Tool{tool},
		HumanGate: func(_ context.Context, req HumanApprovalRequest) (HumanApprovalResponse, error) {
			consulted = append(consulted, req.ToolName)
			return Deny("should not be asked"), nil
		},
		// Flags a different tool, so this one runs without consulting the gate.
		RequiresApproval: func(name string, _ map[string]any) bool { return name == "send_email" },
	})

	res, err := agent.Run(context.Background(), "delete record 7", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(consulted) != 0 {
		t.Fatalf("gate should not be consulted for unflagged tool; consulted=%v", consulted)
	}
	if res.Text != "done" || len(invocations) != 1 {
		t.Fatalf("expected tool to run; result=%+v invocations=%d", res, len(invocations))
	}
}
