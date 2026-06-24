package core

import (
	"context"
	"testing"
)

func TestDelegateToolRunsChildAndReturnsReply(t *testing.T) {
	// The child agent answers the delegated subtask.
	childClient := &fakeClient{scripted: []ChatResponse{{Content: "researched: 42"}}}
	child := NewSmoothAgent(childClient, AgentOptions{Instructions: "researcher"})
	researcher := DelegateTool("researcher", "Delegate a research subtask.", child)

	// The parent calls the delegate tool, then wraps up.
	parentClient := &fakeClient{scripted: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "researcher", Arguments: `{"task":"find the answer"}`}}},
		{Content: "the answer is 42"},
	}}
	parent := NewSmoothAgent(parentClient, AgentOptions{Tools: []Tool{researcher}})

	result, err := parent.Run(context.Background(), "delegate to the researcher", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "the answer is 42" || result.ToolCalls != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestDelegateToolSchemaRequiresTask(t *testing.T) {
	child := NewSmoothAgent(&fakeClient{scripted: []ChatResponse{{Content: "x"}}}, AgentOptions{})
	tool := DelegateTool("helper", "help", child)
	if tool.Name() != "helper" {
		t.Fatalf("name = %q", tool.Name())
	}
	props, ok := tool.Parameters()["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing: %+v", tool.Parameters())
	}
	if _, ok := props["task"]; !ok {
		t.Fatalf("task property missing: %+v", props)
	}
	required, ok := tool.Parameters()["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "task" {
		t.Fatalf("required should be [task]: %+v", tool.Parameters()["required"])
	}
}
