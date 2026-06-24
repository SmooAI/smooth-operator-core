package core

import (
	"context"
	"math"
	"testing"
)

func TestCostTrackerAccumulates(t *testing.T) {
	var tr CostTracker
	pricing := map[string]ModelPricing{"m": {InputPerMTok: 1.0, OutputPerMTok: 2.0}}
	tr.Record("m", Usage{PromptTokens: 1_000_000, CompletionTokens: 500_000}, pricing)
	tr.Record("m", Usage{PromptTokens: 0, CompletionTokens: 500_000}, pricing)
	if tr.Usage.TotalTokens() != 2_000_000 {
		t.Fatalf("total tokens = %d, want 2,000,000", tr.Usage.TotalTokens())
	}
	if math.Abs(tr.CostUSD-3.0) > 1e-9 {
		t.Fatalf("cost = %v, want 3.0", tr.CostUSD)
	}
}

func TestCostTrackerUnknownModelNoCost(t *testing.T) {
	var tr CostTracker
	tr.Record("unknown", Usage{PromptTokens: 100, CompletionTokens: 50}, map[string]ModelPricing{})
	if tr.Usage.TotalTokens() != 150 || tr.CostUSD != 0 {
		t.Fatalf("unknown model: tokens=%d cost=%v", tr.Usage.TotalTokens(), tr.CostUSD)
	}
}

func TestCostTrackerExceeds(t *testing.T) {
	tr := CostTracker{Usage: Usage{PromptTokens: 80, CompletionTokens: 40}, CostUSD: 0.5}
	if tr.Exceeds(nil) {
		t.Fatal("nil budget should never exceed")
	}
	if tr.Exceeds(&CostBudget{MaxTokens: 200}) {
		t.Fatal("120 tokens should not exceed 200")
	}
	if !tr.Exceeds(&CostBudget{MaxTokens: 100}) {
		t.Fatal("120 tokens should exceed 100")
	}
	if tr.Exceeds(&CostBudget{MaxUSD: 1.0}) {
		t.Fatal("0.5 USD should not exceed 1.0")
	}
	if !tr.Exceeds(&CostBudget{MaxUSD: 0.5}) {
		t.Fatal("0.5 USD should exceed 0.5")
	}
}

func TestRunReportsUsageAndCost(t *testing.T) {
	client := &fakeClient{scripted: []ChatResponse{{Content: "hi", Usage: Usage{PromptTokens: 1_000_000, CompletionTokens: 1_000_000}}}}
	agent := NewSmoothAgent(client, AgentOptions{Model: "claude-haiku-4-5"})
	res, err := agent.Run(context.Background(), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.TotalTokens() != 2_000_000 {
		t.Fatalf("usage total = %d", res.Usage.TotalTokens())
	}
	// haiku default pricing = 1.0 in, 5.0 out per 1M.
	if math.Abs(res.CostUSD-6.0) > 1e-9 {
		t.Fatalf("cost = %v, want 6.0", res.CostUSD)
	}
	if res.BudgetExceeded {
		t.Fatal("budget should not be exceeded")
	}
}

func TestRunStopsWhenBudgetExceeded(t *testing.T) {
	client := &fakeClient{scripted: []ChatResponse{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "noop", Arguments: "{}"}}, Usage: Usage{PromptTokens: 200}},
	}}
	agent := NewSmoothAgent(client, AgentOptions{Model: "claude-haiku-4-5", Budget: &CostBudget{MaxTokens: 100}})
	res, err := agent.Run(context.Background(), "go", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.BudgetExceeded || res.Iterations != 1 || res.ToolCalls != 0 {
		t.Fatalf("expected budget stop at iter 1 with 0 tool calls; got %+v", res)
	}
}
